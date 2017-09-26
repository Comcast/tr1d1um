package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
	"net/url"

	"github.com/Comcast/webpa-common/logging"
	"github.com/Comcast/webpa-common/secure"
	"github.com/Comcast/webpa-common/secure/handler"
	"github.com/Comcast/webpa-common/secure/key"
	"github.com/Comcast/webpa-common/server"
	"github.com/SermoDigital/jose/jwt"
	"github.com/go-kit/kit/log"
	"github.com/gorilla/mux"
	"github.com/justinas/alice"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"github.com/Comcast/webpa-common/concurrent"
	"github.com/Comcast/webpa-common/webhook"
)

//convenient global values
const (
	applicationName = "tr1d1um"
	DefaultKeyID    = "current"
	baseURI         = "/api"
	version         = "v2" // TODO: Should these values change?
)

func tr1d1um(arguments []string) (exitCode int) {

	var (
		f                  = pflag.NewFlagSet(applicationName, pflag.ContinueOnError)
		v                  = viper.New()
		logger, webPA, err = server.Initialize(applicationName, arguments, f, v)
	)

	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to initialize viper: %s\n", err.Error())
		return 1
	}
	
	var (
		infoLogger = logging.Info(logger)
	)

	infoLogger.Log("configurationFile", v.ConfigFileUsed())

	tConfig := new(Tr1d1umConfig)
	err = v.Unmarshal(tConfig) //todo: decide best way to get current unexported fields from viper

	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to unmarshall config data into struct: %s\n", err.Error())
		return 1
	}

	preHandler, err := SetUpPreHandler(v, logger)

	if err != nil {
		fmt.Fprintf(os.Stderr, "error setting up prehandler: %s\n", err.Error())
		return 1
	}

	conversionHandler := SetUpHandler(tConfig, logger)
	
	r := mux.NewRouter()

	AddRoutes(r, preHandler, conversionHandler)

	if exitCode = ConfigureWebHooks(r,preHandler,v,logger); exitCode != 0 {
		return
	}

	var (
		_, tr1d1umServer = webPA.Prepare(logger, nil, conversionHandler)
		signals = make(chan os.Signal, 1)
	)

	if err := concurrent.Await(tr1d1umServer, signals); err != nil {
		fmt.Fprintf(os.Stderr, "Error when starting %s: %s", applicationName, err)
		return 4
	}

	return 0
}

//ConfigureWebHooks sets route paths, initializes and synchronizes hook registries for this tr1d1um instance
func ConfigureWebHooks(r *mux.Router, preHandler *alice.Chain, v *viper.Viper, logger log.Logger) int {
	webHookFactory, err := webhook.NewFactory(v)

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating new webHook factory: %s\n", err)
		return 1
	}

	webHookRegistry, webHookHandler := webHookFactory.NewRegistryAndHandler()

	// register webHook end points for api
	r.Handle("/hook", preHandler.ThenFunc(webHookRegistry.UpdateRegistry))
	r.Handle("/hooks", preHandler.ThenFunc(webHookRegistry.GetRegistry))

	selfURL := &url.URL{
		Scheme: "https",
		Host:   v.GetString("fqdn") + v.GetString("primary.address"),
	}

	webHookFactory.Initialize(r, selfURL, webHookHandler, logger, nil)
	webHookFactory.PrepareAndStart()

	startChan := make(chan webhook.Result, 1)
	webHookFactory.Start.GetCurrentSystemsHooks(startChan)

	if webHookStartResults := <-startChan; webHookStartResults.Error == nil {
		webHookFactory.SetList(webhook.NewList(webHookStartResults.Hooks))
	} else {
		logging.Error(logger).Log(logging.ErrorKey(),webHookStartResults.Error)
	}

	return 0
}


//AddRoutes configures the paths and connection rules to TR1D1UM
func AddRoutes(r *mux.Router, preHandler *alice.Chain, conversionHandler *ConversionHandler) *mux.Router {
	var BodyNonNil = func(request *http.Request, match *mux.RouteMatch) bool {
		return request.Body != nil
	}

	apiHandler := r.PathPrefix(fmt.Sprintf("%s/%s", baseURI, version)).Subrouter()

	apiHandler.Handle("/device/{deviceid}/{service}", preHandler.Then(conversionHandler)).
		Methods(http.MethodGet)

	apiHandler.Handle("/device/{deviceid}/{service}", preHandler.Then(conversionHandler)).
		Methods(http.MethodPatch).MatcherFunc(BodyNonNil)

	apiHandler.Handle("/device/{deviceid}/{service}/{parameter}", preHandler.Then(conversionHandler)).
		Methods(http.MethodDelete)

	apiHandler.Handle("/device/{deviceid}/{service}/{parameter}", preHandler.Then(conversionHandler)).
		Methods(http.MethodPut, http.MethodPost).MatcherFunc(BodyNonNil)
		
	return r
}

//SetUpHandler prepares the main handler under TR1D1UM which is the ConversionHandler
func SetUpHandler(tConfig *Tr1d1umConfig, logger log.Logger) (cHandler *ConversionHandler) {
	timeOut, err := time.ParseDuration(tConfig.HTTPTimeout)
	if err != nil {
		timeOut = time.Second * 60 //default val
	}

	cHandler = &ConversionHandler{
		wdmpConvert:    &ConversionWDMP{&EncodingHelper{}},
		sender:         &Tr1SendAndHandle{log: logger, timedClient: &http.Client{Timeout: timeOut},
		NewHTTPRequest: http.NewRequest},
		encodingHelper: &EncodingHelper{},
	}
	//pass loggers
	cHandler.errorLogger = logging.Error(logger)
	cHandler.infoLogger = logging.Info(logger)
	cHandler.targetURL = "https://api-cd.xmidt.comcast.net:8090"
	return
}

//SetUpPreHandler configures the authorization requirements for requests to reach the main handler
func SetUpPreHandler(v *viper.Viper, logger log.Logger) (preHandler *alice.Chain, err error) {
	validator, err := GetValidator(v)
	if err != nil {
		return
	}

	authHandler := handler.AuthorizationHandler{
		HeaderName:          "Authorization",
		ForbiddenStatusCode: 403,
		Validator:           validator,
		Logger:              logger,
	}

	newPreHandler := alice.New(authHandler.Decorate)
	preHandler = &newPreHandler
	return
}

//GetValidator returns a validator for JWT tokens
func GetValidator(v *viper.Viper) (validator secure.Validator, err error) {
	defaultValidators := make(secure.Validators, 0, 0)
	var jwtVals []JWTValidator

	v.UnmarshalKey("jwtValidators", &jwtVals)

	// make sure there is at least one jwtValidator supplied
	if len(jwtVals) < 1 {
		validator = defaultValidators
		return
	}

	// if a JWTKeys section was supplied, configure a JWS validator
	// and append it to the chain of validators
	validators := make(secure.Validators, 0, len(jwtVals))

	for _, validatorDescriptor := range jwtVals {
		var keyResolver key.Resolver
		keyResolver, err = validatorDescriptor.Keys.NewResolver()
		if err != nil {
			validator = validators
			return
		}

		validators = append(
			validators,
			secure.JWSValidator{
				DefaultKeyId:  DefaultKeyID,
				Resolver:      keyResolver,
				JWTValidators: []*jwt.Validator{validatorDescriptor.Custom.New()},
			},
		)
	}

	// TODO: This should really be part of the unmarshalled validators somehow
	basicAuth := v.GetStringSlice("authHeader")
	for _, authValue := range basicAuth {
		validators = append(
			validators,
			secure.ExactMatchValidator(authValue),
		)
	}

	validator = validators

	return
}

func main() {
	os.Exit(tr1d1um(os.Args))
}
