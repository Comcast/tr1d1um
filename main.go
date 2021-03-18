/**
 * Copyright 2017 Comcast Cable Communications Management, LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"time"

	"github.com/xmidt-org/tr1d1um/common"
	"github.com/xmidt-org/tr1d1um/stat"
	"github.com/xmidt-org/tr1d1um/translation"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/goph/emperror"
	"github.com/gorilla/mux"
	"github.com/justinas/alice"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"github.com/xmidt-org/ancla"
	"github.com/xmidt-org/bascule"
	"github.com/xmidt-org/bascule/acquire"
	"github.com/xmidt-org/bascule/basculehttp"
	"github.com/xmidt-org/bascule/key"
	"github.com/xmidt-org/candlelight"
	"github.com/xmidt-org/webpa-common/basculechecks"
	"github.com/xmidt-org/webpa-common/basculemetrics"
	"github.com/xmidt-org/webpa-common/concurrent"
	"github.com/xmidt-org/webpa-common/logging"
	"github.com/xmidt-org/webpa-common/server"
	"github.com/xmidt-org/webpa-common/xhttp"
	"github.com/xmidt-org/webpa-common/xmetrics"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gorilla/mux/otelmux"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// convenient global values
const (
	DefaultKeyID             = "current"
	applicationName, apiBase = "tr1d1um", "api/v2"
)

const (
	translationServicesKey            = "supportedServices"
	targetURLKey                      = "targetURL"
	netDialerTimeoutKey               = "netDialerTimeout"
	clientTimeoutKey                  = "clientTimeout"
	reqTimeoutKey                     = "respWaitTimeout"
	reqRetryIntervalKey               = "requestRetryInterval"
	reqMaxRetriesKey                  = "requestMaxRetries"
	wrpSourceKey                      = "WRPSource"
	hooksSchemeKey                    = "hooksScheme"
	reducedTransactionLoggingCodesKey = "log.reducedLoggingResponseCodes"
	authAcquirerKey                   = "authAcquirer"
	webhookConfigKey                  = "webhook"
	tracingConfigKey                  = "tracing"
)

var (
	// dynamic versioning
	Version   string
	BuildTime string
	GitCommit string
)

var defaults = map[string]interface{}{
	translationServicesKey: []string{}, // no services allowed by the default
	targetURLKey:           "localhost:6000",
	netDialerTimeoutKey:    "5s",
	clientTimeoutKey:       "50s",
	reqTimeoutKey:          "40s",
	reqRetryIntervalKey:    "2s",
	reqMaxRetriesKey:       2,
	wrpSourceKey:           "dns:localhost",
	hooksSchemeKey:         "https",
}

type firstNodeTraceEchoer struct {
	propagator propagation.TextMapPropagator
}

// EchoStartNodeTrace should be run after the middleware that starts the main span of this application.
// Assuming this is the first node of the entire trace, the trace information is echoed through the
// HTTP response.
// This would probably need to be in xmidt-org/candlelight
func (f firstNodeTraceEchoer) EchoStartNodeTrace(delegate http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := f.propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		rsc := trace.RemoteSpanContextFromContext(ctx)
		if !rsc.IsValid() {
			sc := trace.SpanContextFromContext(ctx)
			w.Header().Set("X-Midt-Span-ID", sc.SpanID.String())
			w.Header().Set("X-Midt-Trace-ID", sc.TraceID.String())
		}
		delegate.ServeHTTP(w, r.WithContext(ctx))
	})
}

func tr1d1um(arguments []string) (exitCode int) {

	var (
		f, v                                = pflag.NewFlagSet(applicationName, pflag.ContinueOnError), viper.New()
		logger, metricsRegistry, webPA, err = server.Initialize(applicationName, arguments, f, v, ancla.Metrics, basculechecks.Metrics, basculemetrics.Metrics)
	)

	// This allows us to communicate the version of the binary upon request.
	if parseErr, done := printVersion(f, arguments); done {
		// if we're done, we're exiting no matter what
		exitIfError(logger, emperror.Wrap(parseErr, "failed to parse arguments"))
		os.Exit(0)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to initialize viper: %s\n", err.Error())
		return 1
	}

	var (
		infoLogger, errorLogger = logging.Info(logger), logging.Error(logger)
		authenticate            *alice.Chain
	)

	for k, va := range defaults {
		v.SetDefault(k, va)
	}

	infoLogger.Log("configurationFile", v.ConfigFileUsed())

	u := v.Sub(tracingConfigKey)
	if u == nil {
		fmt.Fprintf(os.Stderr, "tracing configuration is missing.\n")
		return 1
	}
	config := &candlelight.Config{
		ApplicationName: applicationName,
	}
	u.Unmarshal(config)
	traceProvider, err := candlelight.ConfigureTracerProvider(*config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to build traceProvider: %s\n", err.Error())
		return 1
	}
	propagator := propagation.TraceContext{}
	traceMiddleware := firstNodeTraceEchoer{propagator: propagator}
	authenticate, err = authenticationHandler(v, logger, metricsRegistry)

	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to build authentication handler: %s\n", err.Error())
		return 1
	}

	rootRouter := mux.NewRouter()
	otelMuxOptions := []otelmux.Option{
		otelmux.WithPropagators(propagator),
		otelmux.WithTracerProvider(traceProvider),
	}
	rootRouter.Use(otelmux.Middleware("mainSpan", otelMuxOptions...), traceMiddleware.EchoStartNodeTrace)

	APIRouter := rootRouter.PathPrefix(fmt.Sprintf("/%s/", apiBase)).Subrouter()

	tConfigs, err := newTimeoutConfigs(v)

	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to parse timeout configuration values: %s \n", err.Error())
		return 1
	}

	//
	// Webhooks (if not configured, handlers are not set up)
	//
	if v.IsSet(webhookConfigKey) {
		var webhookConfig ancla.Config
		err := v.UnmarshalKey(webhookConfigKey, &webhookConfig)

		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to decode config for webhook service: %s\n", err.Error())
			return 1
		}

		webhookConfig.Logger = logger
		webhookConfig.MetricsProvider = metricsRegistry

		svc, stopWatch, err := ancla.Initialize(webhookConfig, GetLogger)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to initialize webhook service: %s\n", err.Error())
			return 1
		}
		defer stopWatch()

		addWebhookHandler := ancla.NewAddWebhookHandler(svc, ancla.HandlerConfig{MetricsProvider: metricsRegistry})
		getAllWebhooksHandler := ancla.NewGetAllWebhooksHandler(svc)

		APIRouter.Handle("/hook", authenticate.Then(addWebhookHandler)).Methods(http.MethodPost)
		APIRouter.Handle("/hooks", authenticate.Then(getAllWebhooksHandler)).Methods(http.MethodGet)

		infoLogger.Log(logging.MessageKey(), "Webhook service enabled")
	} else {
		infoLogger.Log(logging.MessageKey(), "Webhook service disabled")
	}

	//
	// Stat Service configs
	//
	statServiceOptions := &stat.ServiceOptions{
		HTTPTransactor: common.NewTr1d1umTransactor(
			&common.Tr1d1umTransactorOptions{
				Do: xhttp.RetryTransactor(
					xhttp.RetryOptions{
						Logger:   logger,
						Retries:  v.GetInt(reqMaxRetriesKey),
						Interval: v.GetDuration(reqRetryIntervalKey),
					},
					newClient(v, tConfigs).Do),
				RequestTimeout: tConfigs.rTimeout,
			}),
		XmidtStatURL: fmt.Sprintf("%s/%s/device/${device}/stat", v.GetString(targetURLKey), apiBase),
	}

	//
	// WRP Service configs
	//
	translationOptions := &translation.ServiceOptions{
		XmidtWrpURL: fmt.Sprintf("%s/%s/device", v.GetString(targetURLKey), apiBase),

		WRPSource: v.GetString(wrpSourceKey),

		Tr1d1umTransactor: common.NewTr1d1umTransactor(
			&common.Tr1d1umTransactorOptions{
				RequestTimeout: tConfigs.rTimeout,
				Do: xhttp.RetryTransactor(
					xhttp.RetryOptions{
						Logger:   logger,
						Retries:  v.GetInt(reqMaxRetriesKey),
						Interval: v.GetDuration(reqRetryIntervalKey),
					},
					newClient(v, tConfigs).Do),
			}),
	}

	reducedLoggingResponseCodes := v.GetIntSlice(reducedTransactionLoggingCodesKey)

	if v.IsSet(authAcquirerKey) {
		acquirer, err := createAuthAcquirer(v)
		if err != nil {
			errorLogger.Log(logging.MessageKey(), "Could not configure auth acquirer", logging.ErrorKey(), err)
		} else {
			translationOptions.AuthAcquirer = acquirer
			statServiceOptions.AuthAcquirer = acquirer
			infoLogger.Log(logging.MessageKey(), "Outbound request authentication token acquirer enabled")
		}
	}

	ss := stat.NewService(statServiceOptions)
	ts := translation.NewService(translationOptions)

	// Must be called before translation.ConfigHandler due to mux path specificity (https://github.com/gorilla/mux#matching-routes).
	stat.ConfigHandler(&stat.Options{
		S:                           ss,
		APIRouter:                   APIRouter,
		Authenticate:                authenticate,
		Log:                         logger,
		ReducedLoggingResponseCodes: reducedLoggingResponseCodes,
	})

	translation.ConfigHandler(&translation.Options{
		S:                           ts,
		APIRouter:                   APIRouter,
		Authenticate:                authenticate,
		Log:                         logger,
		ValidServices:               v.GetStringSlice(translationServicesKey),
		ReducedLoggingResponseCodes: reducedLoggingResponseCodes,
	})

	var (
		_, tr1d1umServer, done = webPA.Prepare(logger, nil, metricsRegistry, r)
		signals                = make(chan os.Signal, 10)
	)

	//
	// Execute the runnable, which runs all the servers, and wait for a signal
	//
	waitGroup, shutdown, err := concurrent.Execute(tr1d1umServer)

	if err != nil {
		errorLogger.Log(logging.MessageKey(), "Unable to start tr1d1um", logging.ErrorKey(), err)
		return 4
	}

	signal.Notify(signals, os.Kill, os.Interrupt)
	for exit := false; !exit; {
		select {
		case s := <-signals:
			logger.Log(level.Key(), level.ErrorValue(), logging.MessageKey(), "exiting due to signal", "signal", s)
			exit = true
		case <-done:
			logger.Log(level.Key(), level.ErrorValue(), logging.MessageKey(), "one or more servers exited")
			exit = true
		}
	}

	close(shutdown)
	waitGroup.Wait()

	return 0
}

// timeoutConfigs holds parsable config values for HTTP transactions
type timeoutConfigs struct {
	// HTTP client timeout
	cTimeout time.Duration

	// HTTP request timeout
	rTimeout time.Duration

	// net dialer timeout
	dTimeout time.Duration
}

func createAuthAcquirer(v *viper.Viper) (acquire.Acquirer, error) {
	var options authAcquirerConfig
	err := v.UnmarshalKey(authAcquirerKey, &options)

	if err != nil {
		return nil, err
	}

	if options.JWT.AuthURL != "" && options.JWT.Buffer != 0 && options.JWT.Timeout != 0 {
		return acquire.NewRemoteBearerTokenAcquirer(options.JWT)
	}

	if options.Basic != "" {
		return acquire.NewFixedAuthAcquirer(options.Basic)
	}

	return nil, errors.New("auth acquirer not configured properly")
}

func newTimeoutConfigs(v *viper.Viper) (t *timeoutConfigs, err error) {
	var c, r, d time.Duration
	if c, err = time.ParseDuration(v.GetString(clientTimeoutKey)); err == nil {
		if r, err = time.ParseDuration(v.GetString(reqTimeoutKey)); err == nil {
			if d, err = time.ParseDuration(v.GetString(netDialerTimeoutKey)); err == nil {
				t = &timeoutConfigs{
					cTimeout: c,
					rTimeout: r,
					dTimeout: d,
				}
			}
		}
	}
	return
}

func newClient(v *viper.Viper, t *timeoutConfigs) *http.Client {
	return &http.Client{
		Timeout: t.cTimeout,
		Transport: &http.Transport{
			Dial: (&net.Dialer{
				Timeout: t.dTimeout,
			}).Dial},
	}
}

func SetLogger(logger log.Logger) func(delegate http.Handler) http.Handler {
	return func(delegate http.Handler) http.Handler {
		return http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				traceId, spanId := candlelight.ExtractTraceInformation(r.Context())
				ctx := r.WithContext(logging.WithLogger(r.Context(),
					log.With(logger, "requestHeaders", r.Header, "requestURL", r.URL.EscapedPath(), "method", r.Method, candlelight.SpanIDLogKeyName, spanId, candlelight.TraceIdLogKeyName, traceId)))
				delegate.ServeHTTP(w, ctx)
			})
	}
}

func GetLogger(ctx context.Context) bascule.Logger {
	logger := log.With(logging.GetLogger(ctx), "ts", log.DefaultTimestampUTC, "caller", log.DefaultCaller)
	return logger
}

// JWTValidator provides a convenient way to define jwt validator through config files
type JWTValidator struct {
	// JWTKeys is used to create the key.Resolver for JWT verification keys
	Keys key.ResolverFactory `json:"keys"`

	// Leeway is used to set the amount of time buffer should be given to JWT
	// time values, such as nbf
	Leeway bascule.Leeway
}

type authAcquirerConfig struct {
	JWT   acquire.RemoteBearerTokenAcquirerOptions
	Basic string
}

type CapabilityConfig struct {
	Type            string
	Prefix          string
	AcceptAllMethod string
	EndpointBuckets []string
}

// authenticationHandler configures the authorization requirements for requests to reach the main handler
func authenticationHandler(v *viper.Viper, logger log.Logger, registry xmetrics.Registry) (*alice.Chain, error) {
	if registry == nil {
		return nil, errors.New("nil registry")
	}

	basculeMeasures := basculemetrics.NewAuthValidationMeasures(registry)
	capabilityCheckMeasures := basculechecks.NewAuthCapabilityCheckMeasures(registry)
	listener := basculemetrics.NewMetricListener(basculeMeasures)

	basicAllowed := make(map[string]string)
	basicAuth := v.GetStringSlice("authHeader")
	for _, a := range basicAuth {
		decoded, err := base64.StdEncoding.DecodeString(a)
		if err != nil {
			logging.Info(logger).Log(logging.MessageKey(), "failed to decode auth header", "authHeader", a, logging.ErrorKey(), err.Error())
		}

		i := bytes.IndexByte(decoded, ':')
		logging.Debug(logger).Log(logging.MessageKey(), "decoded string", "string", decoded, "i", i)
		if i > 0 {
			basicAllowed[string(decoded[:i])] = string(decoded[i+1:])
		}
	}
	logging.Debug(logger).Log(logging.MessageKey(), "Created list of allowed basic auths", "allowed", basicAllowed, "config", basicAuth)

	options := []basculehttp.COption{
		basculehttp.WithCLogger(GetLogger),
		basculehttp.WithCErrorResponseFunc(listener.OnErrorResponse),
		basculehttp.WithParseURLFunc(basculehttp.CreateRemovePrefixURLFunc("/"+apiBase+"/", basculehttp.DefaultParseURLFunc)),
	}
	if len(basicAllowed) > 0 {
		options = append(options, basculehttp.WithTokenFactory("Basic", basculehttp.BasicTokenFactory(basicAllowed)))
	}
	var jwtVal JWTValidator

	v.UnmarshalKey("jwtValidator", &jwtVal)
	if jwtVal.Keys.URI != "" {
		resolver, err := jwtVal.Keys.NewResolver()
		if err != nil {
			return &alice.Chain{}, emperror.With(err, "failed to create resolver")
		}

		options = append(options, basculehttp.WithTokenFactory("Bearer", basculehttp.BearerTokenFactory{
			DefaultKeyId: DefaultKeyID,
			Resolver:     resolver,
			Parser:       bascule.DefaultJWTParser,
			Leeway:       jwtVal.Leeway,
		}))
	}

	authConstructor := basculehttp.NewConstructor(options...)

	bearerRules := bascule.Validators{
		bascule.CreateNonEmptyPrincipalCheck(),
		bascule.CreateNonEmptyTypeCheck(),
		bascule.CreateValidTypeCheck([]string{"jwt"}),
	}

	// only add capability check if the configuration is set
	var capabilityCheck CapabilityConfig
	v.UnmarshalKey("capabilityCheck", &capabilityCheck)
	if capabilityCheck.Type == "enforce" || capabilityCheck.Type == "monitor" {
		var endpoints []*regexp.Regexp
		c, err := basculechecks.NewEndpointRegexCheck(capabilityCheck.Prefix, capabilityCheck.AcceptAllMethod)
		if err != nil {
			return nil, emperror.With(err, "failed to create capability check")
		}
		for _, e := range capabilityCheck.EndpointBuckets {
			r, err := regexp.Compile(e)
			if err != nil {
				logging.Error(logger).Log(logging.MessageKey(), "failed to compile regular expression", "regex", e, logging.ErrorKey(), err.Error())
				continue
			}
			endpoints = append(endpoints, r)
		}
		m := basculechecks.MetricValidator{
			C:         basculechecks.CapabilitiesValidator{Checker: c},
			Measures:  capabilityCheckMeasures,
			Endpoints: endpoints,
		}
		bearerRules = append(bearerRules, m.CreateValidator(capabilityCheck.Type == "enforce"))
	}

	authEnforcer := basculehttp.NewEnforcer(
		basculehttp.WithELogger(GetLogger),
		basculehttp.WithRules("Basic", bascule.Validators{
			bascule.CreateAllowAllCheck(),
		}),
		basculehttp.WithRules("Bearer", bearerRules),
		basculehttp.WithEErrorResponseFunc(listener.OnErrorResponse),
	)

	constructors := []alice.Constructor{SetLogger(logger), authConstructor, authEnforcer, basculehttp.NewListenerDecorator(listener)}

	chain := alice.New(constructors...)
	return &chain, nil
}

func printVersion(f *pflag.FlagSet, arguments []string) (error, bool) {
	printVer := f.BoolP("version", "v", false, "displays the version number")
	if err := f.Parse(arguments); err != nil {
		return err, true
	}

	if *printVer {
		printVersionInfo(os.Stdout)
		return nil, true
	}
	return nil, false
}

func exitIfError(logger log.Logger, err error) {
	if err != nil {
		if logger != nil {
			logging.Error(logger, emperror.Context(err)...).Log(logging.ErrorKey(), err.Error())
		}
		fmt.Fprintf(os.Stderr, "Error: %#v\n", err.Error())
		os.Exit(1)
	}
}

func printVersionInfo(writer io.Writer) {
	fmt.Fprintf(writer, "%s:\n", applicationName)
	fmt.Fprintf(writer, "  version: \t%s\n", Version)
	fmt.Fprintf(writer, "  go version: \t%s\n", runtime.Version())
	fmt.Fprintf(writer, "  built time: \t%s\n", BuildTime)
	fmt.Fprintf(writer, "  git commit: \t%s\n", GitCommit)
	fmt.Fprintf(writer, "  os/arch: \t%s/%s\n", runtime.GOOS, runtime.GOARCH)
}

func main() {
	os.Exit(tr1d1um(os.Args))
}
