// Copyright © 2019 by PACE Telematics GmbH. All rights reserved.
// Created at 2019/10/18 Charlotte Pröller

package servicehealthcheck

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/caarlos0/env"
	"github.com/opentracing/opentracing-go"

	"github.com/pace/bricks/maintenance/errors"
	"github.com/pace/bricks/maintenance/log"
)

// HealthCheck is a health check that is registered once and that is performed
// periodically and/or spontaneously.
type HealthCheck interface {
	HealthCheck(ctx context.Context) HealthCheckResult
}

type HealthCheckFunc func(ctx context.Context) HealthCheckResult

func (hcf HealthCheckFunc) HealthCheck(ctx context.Context) HealthCheckResult {
	return hcf(ctx)
}

// Initializable is used to mark that a health check needs to be initialized
type Initializable interface {
	Init(ctx context.Context) error
}

type config struct {
	// Amount of time to cache the last init
	HealthCheckInitResultErrorTTL time.Duration `env:"HEALTH_CHECK_INIT_RESULT_ERROR_TTL" envDefault:"10s"`
	// Amount of time to wait before failing the health check
	HealthCheckMaxWait time.Duration `env:"HEALTH_CHECK_MAX_WAIT" envDefault:"5s"`
}

var cfg config

// requiredChecks contains all required registered Health Checks - key:Name
var requiredChecks sync.Map

// optionalChecks contains all optional registered Health Checks - key:Name
var optionalChecks sync.Map

// initErrors map with all err ConnectionState that happened in the initialization of any health check - key:Name
var initErrors sync.Map

// HealthState describes if a any error or warning occurred during the health check of a service
type HealthState string

const (
	// Err State of a service, if an error occurred during the health check of the service
	Err HealthState = "ERR"
	// Warn State of a service, if a warning occurred during the health check of the service
	Warn HealthState = "WARN"
	// Ok State of a service, if no warning or error occurred during the health check of the service
	Ok HealthState = "OK"
)

// HealthCheckResult describes the result of a health check, contains the state of a service and a message that
// describes the state. If the state is Ok the description can be empty.
// The description should contain the error message if any error or warning occurred during the health check.
type HealthCheckResult struct {
	State HealthState
	Msg   string
}

func init() {
	err := env.Parse(&cfg)
	if err != nil {
		log.Fatalf("Failed to parse health check environment: %v", err)
	}
}

func check(ctx context.Context, hcs *sync.Map) map[string]HealthCheckResult {
	span, ctx := opentracing.StartSpanFromContext(ctx, "HealthCheck")
	defer span.Finish()

	result := make(map[string]HealthCheckResult)
	var resultSync sync.Map
	var wg sync.WaitGroup

	hcs.Range(func(key, value interface{}) bool {
		name := key.(string)
		hc := value.(healthCheck)
		ctx, cancel := context.WithTimeout(ctx, hc.maxWait)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer cancel()

			defer errors.HandleWithCtx(ctx, fmt.Sprintf("HealthCheck %s", name))
			span, ctx := opentracing.StartSpanFromContext(ctx, fmt.Sprintf("HealthCheck %s", name))
			defer span.Finish()

			// If Init failed, try again
			if val, ok := initErrors.Load(name); ok {
				state := val.(*ConnectionState)
				if time.Since(state.LastChecked()) < hc.initResultErrorTTL {
					// Too soon, return same state
					resultSync.Store(name, state.GetState())
					return
				}

				initErr := hc.check.(Initializable).Init(ctx)
				if initErr != nil {
					// Init failed, update init state err and return it
					state.SetErrorState(initErr)
					resultSync.Store(name, state.GetState())
					return
				}

				// Init succeeded, clear init state error, and proceed with check
				initErrors.Delete(name)
			}
			// this is the actual health check
			resultSync.Store(name, hc.check.HealthCheck(ctx))
		}()
		return true
	})
	wg.Wait()
	resultSync.Range(func(key, value interface{}) bool {
		result[key.(string)] = value.(HealthCheckResult)
		return true
	})

	return result
}

func writeResult(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(status)
	if _, err := fmt.Fprint(w, body); err != nil {
		log.Warnf("could not write output: %s", err)
	}
}

type healthCheck struct {
	check                   HealthCheck
	initResultErrorTTL      time.Duration
	maxWait                 time.Duration
	runInBackgroundInterval time.Duration
}

type HealthCheckOption func(cfg *healthCheck)

func UseInitErrResultTTL(ttl time.Duration) HealthCheckOption {
	return func(cfg *healthCheck) {
		cfg.initResultErrorTTL = ttl
	}
}

func UseMaxWait(maxWait time.Duration) HealthCheckOption {
	return func(cfg *healthCheck) {
		cfg.maxWait = maxWait
	}
}

func RunInBackgroundAtInterval(interval time.Duration) HealthCheckOption {
	return func(cfg *healthCheck) {
		cfg.runInBackgroundInterval = interval
	}
}

// RegisterHealthCheck registers a required HealthCheck. The name
// must be unique. If the health check satisfies the Initializable interface, it
// is initialized before it is added.
// It is not possible to add a health check with the same name twice, even if one is required and one is optional
func RegisterHealthCheck(name string, hc HealthCheck, opts ...HealthCheckOption) {
	registerHealthCheck(&requiredChecks, hc, name, opts...)
}

// RegisterHealthCheckFunc registers a required HealthCheck. The name
// must be unique.  It is not possible to add a health check with the same name twice,
// even if one is required and one is optional
func RegisterHealthCheckFunc(name string, f HealthCheckFunc, opts ...HealthCheckOption) {
	RegisterHealthCheck(name, f, opts...)
}

// RegisterOptionalHealthCheck registers a HealthCheck like RegisterHealthCheck(hc HealthCheck, name string)
// but the health check is only checked for /health/check and not for /health/
func RegisterOptionalHealthCheck(hc HealthCheck, name string, opts ...HealthCheckOption) {
	registerHealthCheck(&optionalChecks, hc, name, opts...)
}

func registerHealthCheck(checks *sync.Map, check HealthCheck, name string, opts ...HealthCheckOption) {
	ctx := log.Logger().WithContext(context.Background())

	// check both lists, because
	if _, inReq := requiredChecks.Load(name); inReq {
		log.Warnf("tried to register health check with name %q twice", name)
		return
	}
	if _, inOpt := optionalChecks.Load(name); inOpt {
		log.Warnf("tried to register health check with name %q twice", name)
		return
	}

	hc := healthCheck{
		check:              check,
		initResultErrorTTL: cfg.HealthCheckInitResultErrorTTL,
		maxWait:            cfg.HealthCheckMaxWait,
	}
	for _, o := range opts {
		o(&hc)
	}

	if hc.runInBackgroundInterval > 0 {
		// registerBackgroundHealthCheck returns a backgroundStateHealthChecker,
		// which will be used instead to check the state, and the original health check
		// will run in the background.
		// Also, initialization + retries are done in the background.
		hc.check = registerBackgroundHealthCheck(name, hc)

	} else if initHC, ok := hc.check.(Initializable); ok {
		if err := initHC.Init(ctx); err != nil {
			log.Warnf("error initializing health check %q: %s", name, err)
			initErrors.Store(name, &ConnectionState{
				lastCheck: time.Now(),
				result: HealthCheckResult{
					State: Err,
					Msg:   err.Error(),
				},
			})
		}
	}
	// save the length of the longest health check name, for the width of the column in /health/check
	if len(name) > longestCheckName {
		longestCheckName = len(name)
	}
	checks.Store(name, hc)
}

// HealthHandler returns the health endpoint for transactional processing. This Handler only checks
// the required health checks and returns ERR and 503 or OK and 200.
func HealthHandler() http.Handler {
	return &healthHandler{}
}

// ReadableHealthHandler returns the health endpoint with all details about service health. This handler checks
// all health checks. The response body contains two tables (for required and optional health checks)
// with the detailed results of the health checks.
func ReadableHealthHandler() http.Handler {
	return &readableHealthHandler{}
}

// JSONHealthHandler return health endpoint with all details about service health. This handler checks
// all health checks. The response body contains a JSON formatted array with every service (required or optional)
// and the detailed health checks about them.
func JSONHealthHandler() http.Handler {
	return &jsonHealthHandler{}
}
