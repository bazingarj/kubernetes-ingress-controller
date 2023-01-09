package sendconfig

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/kong/deck/diff"
	"github.com/kong/deck/dump"
	"github.com/kong/deck/file"
	"github.com/kong/deck/state"
	deckutils "github.com/kong/deck/utils"
	"github.com/kong/go-kong/kong"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"

	"github.com/kong/kubernetes-ingress-controller/v2/internal/dataplane/deckgen"
	"github.com/kong/kubernetes-ingress-controller/v2/internal/metrics"
)

const initialHash = "00000000000000000000000000000000"

// -----------------------------------------------------------------------------
// Sendconfig - Public Functions
// -----------------------------------------------------------------------------

// PerformUpdate writes `targetContent` to Kong Admin API specified by `kongConfig`.
func PerformUpdate(ctx context.Context,
	log logrus.FieldLogger,
	kongConfig *Kong,
	inMemory bool,
	reverseSync bool,
	skipCACertificates bool,
	targetContent *file.Content,
	selectorTags []string,
	oldSHA []byte,
	promMetrics *metrics.CtrlFuncMetrics,
) ([]byte, error) {
	newSHA, err := deckgen.GenerateSHA(targetContent)
	if err != nil {
		return oldSHA, err
	}
	// disable optimization if reverse sync is enabled
	if !reverseSync {
		// use the previous SHA to determine whether or not to perform an update
		if equalSHA(oldSHA, newSHA) {
			if !hasSHAUpdateAlreadyBeenReported(newSHA) {
				log.Debugf("sha %s has been reported", hex.EncodeToString(newSHA))
			}
			// we assume ready as not all Kong versions provide their configuration hash, and their readiness state
			// is always unknown
			ready := true
			status, err := kongConfig.Client.Status(ctx)
			if err != nil {
				log.WithError(err).Error("checking config status failed")
				log.Debug("configuration state unknown, skipping sync to kong")
				return oldSHA, nil
			}
			if status.ConfigurationHash == initialHash {
				ready = false
			}
			if ready {
				log.Debug("no configuration change, skipping sync to kong")
				return oldSHA, nil
			}
		}
	}

	var metricsProtocol string
	timeStart := time.Now()
	if inMemory {
		metricsProtocol = metrics.ProtocolDBLess
		err = onUpdateInMemoryMode(ctx, log, targetContent, kongConfig)
	} else {
		metricsProtocol = metrics.ProtocolDeck
		err = onUpdateDBMode(ctx, targetContent, kongConfig, selectorTags, skipCACertificates)
	}
	timeEnd := time.Now()

	if err != nil {
		promMetrics.ConfigPushCount.With(prometheus.Labels{
			metrics.SuccessKey:       metrics.SuccessFalse,
			metrics.ProtocolKey:      metricsProtocol,
			metrics.FailureReasonKey: pushFailureReason(err),
		}).Inc()
		promMetrics.ConfigPushDuration.With(prometheus.Labels{
			metrics.SuccessKey:  metrics.SuccessFalse,
			metrics.ProtocolKey: metricsProtocol,
		}).Observe(float64(timeEnd.Sub(timeStart).Milliseconds()))
		return nil, err
	}

	promMetrics.ConfigPushCount.With(prometheus.Labels{
		metrics.SuccessKey:       metrics.SuccessTrue,
		metrics.ProtocolKey:      metricsProtocol,
		metrics.FailureReasonKey: "",
	}).Inc()
	promMetrics.ConfigPushDuration.With(prometheus.Labels{
		metrics.SuccessKey:  metrics.SuccessTrue,
		metrics.ProtocolKey: metricsProtocol,
	}).Observe(float64(timeEnd.Sub(timeStart).Milliseconds()))
	log.Info("successfully synced configuration to kong.")
	return newSHA, nil
}

// -----------------------------------------------------------------------------
// Sendconfig - Private Functions
// -----------------------------------------------------------------------------

func onUpdateInMemoryMode(ctx context.Context,
	log logrus.FieldLogger,
	state *file.Content,
	kongConfig *Kong,
) error {
	// Kong will error out if this is set
	state.Info = nil
	// Kong errors out if `null`s are present in `config` of plugins
	deckgen.CleanUpNullsInPluginConfigs(state)

	config, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("constructing kong configuration: %w", err)
	}

	req, err := http.NewRequest("POST", kongConfig.URL+"/config",
		bytes.NewReader(config))
	if err != nil {
		return fmt.Errorf("creating new HTTP request for /config: %w", err)
	}
	req.Header.Add("content-type", "application/json")

	queryString := req.URL.Query()
	queryString.Add("check_hash", "1")

	req.URL.RawQuery = queryString.Encode()

	_, err = kongConfig.Client.Do(ctx, req, nil)
	if err != nil {
		return fmt.Errorf("posting new config to /config: %w", err)
	}

	return err
}

func onUpdateDBMode(ctx context.Context,
	targetContent *file.Content,
	kongConfig *Kong,
	selectorTags []string,
	skipCACertificates bool,
) error {
	dumpConfig := dump.Config{SelectorTags: selectorTags, SkipCACerts: skipCACertificates}

	cs, err := currentState(ctx, kongConfig, dumpConfig)
	if err != nil {
		return err
	}

	ts, err := targetState(ctx, targetContent, cs, kongConfig, dumpConfig)
	if err != nil {
		return deckConfigConflictError{err}
	}

	syncer, err := diff.NewSyncer(diff.SyncerOpts{
		CurrentState:    cs,
		TargetState:     ts,
		KongClient:      kongConfig.Client,
		SilenceWarnings: true,
	})
	if err != nil {
		return fmt.Errorf("creating a new syncer: %w", err)
	}

	_, errs := syncer.Solve(ctx, kongConfig.Concurrency, false)
	if errs != nil {
		return deckutils.ErrArray{Errors: errs}
	}
	return nil
}

func currentState(ctx context.Context, kongConfig *Kong, dumpConfig dump.Config) (*state.KongState, error) {
	rawState, err := dump.Get(ctx, kongConfig.Client, dumpConfig)
	if err != nil {
		return nil, fmt.Errorf("loading configuration from kong: %w", err)
	}

	return state.Get(rawState)
}

func targetState(ctx context.Context, targetContent *file.Content, currentState *state.KongState, kongConfig *Kong, dumpConfig dump.Config) (*state.KongState, error) {
	rawState, err := file.Get(ctx, targetContent, file.RenderConfig{
		CurrentState: currentState,
		KongVersion:  kongConfig.Version,
	}, dumpConfig, kongConfig.Client)
	if err != nil {
		return nil, err
	}

	return state.Get(rawState)
}

func equalSHA(a, b []byte) bool {
	return reflect.DeepEqual(a, b)
}

var (
	latestReportedSHA []byte
	shaLock           sync.RWMutex
)

// hasSHAUpdateAlreadyBeenReported is a helper function to allow
// sendconfig internals to be aware of the last logged/reported
// update to the Kong Admin API. Given the most recent update SHA,
// it will return true/false whether or not that SHA has previously
// been reported (logged, e.t.c.) so that the caller can make
// decisions (such as staggering or stifling duplicate log lines).
//
// TODO: This is a bit of a hack for now to keep backwards compat,
//
//	but in the future we might configure rolling this into
//	some object/interface which has this functionality as an
//	inherent behavior.
func hasSHAUpdateAlreadyBeenReported(latestUpdateSHA []byte) bool {
	shaLock.Lock()
	defer shaLock.Unlock()
	if equalSHA(latestReportedSHA, latestUpdateSHA) {
		return true
	}
	latestReportedSHA = latestUpdateSHA
	return false
}

// deckConfigConflictError is an error used to wrap deck config conflict errors returned from deck functions
// transforming KongRawState to KongState (e.g. state.Get, dump.Get).
type deckConfigConflictError struct {
	err error
}

func (e deckConfigConflictError) Error() string {
	return e.err.Error()
}

func (e deckConfigConflictError) Is(target error) bool {
	_, ok := target.(deckConfigConflictError)
	return ok
}

func (e deckConfigConflictError) Unwrap() error {
	return e.err
}

// pushFailureReason extracts config push failure reason from an error returned from onUpdateInMemoryMode or onUpdateDBMode.
func pushFailureReason(err error) string {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return metrics.FailureReasonNetwork
	}

	if isConflictErr(err) {
		return metrics.FailureReasonConflict
	}

	return metrics.FailureReasonOther
}

func isConflictErr(err error) bool {
	var apiErr *kong.APIError
	if errors.As(err, &apiErr) && apiErr.Code() == http.StatusConflict ||
		errors.Is(err, deckConfigConflictError{}) {
		return true
	}

	var deckErrArray deckutils.ErrArray
	if errors.As(err, &deckErrArray) {
		for _, err := range deckErrArray.Errors {
			if isConflictErr(err) {
				return true
			}
		}
	}

	return false
}
