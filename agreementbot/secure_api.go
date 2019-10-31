package agreementbot

import (
	"fmt"
	"github.com/golang/glog"
	"github.com/gorilla/mux"
	"github.com/open-horizon/anax/agreementbot/persistence"
	"github.com/open-horizon/anax/config"
	"github.com/open-horizon/anax/cutil"
	"github.com/open-horizon/anax/events"
	"github.com/open-horizon/anax/exchange"
	"github.com/open-horizon/anax/policy"
	"github.com/open-horizon/anax/worker"
	"net/http"
	"os"
	"strings"
	"time"
)

type SecureAPI struct {
	worker.Manager // embedded field
	name           string
	db             persistence.AgbotDatabase
	httpClient     *http.Client // a shared HTTP client instance for this worker
	pm             *policy.PolicyManager
	EC             *worker.BaseExchangeContext
	em             *events.EventStateManager
	shutdownError  string
}

func NewSecureAPIListener(name string, config *config.HorizonConfig, db persistence.AgbotDatabase, configFile string) *SecureAPI {
	messages := make(chan events.Message)

	listener := &SecureAPI{
		Manager: worker.Manager{
			Config:   config,
			Messages: messages,
		},
		httpClient: config.Collaborators.HTTPClientFactory.NewHTTPClient(nil),
		name:       name,
		db:         db,
		EC:         worker.NewExchangeContext(config.AgreementBot.ExchangeId, config.AgreementBot.ExchangeToken, config.AgreementBot.ExchangeURL, config.GetAgbotCSSURL(), config.Collaborators.HTTPClientFactory),
		em:         events.NewEventStateManager(),
	}

	listener.listen()
	return listener
}

// Worker framework functions
func (a *SecureAPI) Messages() chan events.Message {
	return a.Manager.Messages
}

func (a *SecureAPI) NewEvent(ev events.Message) {

	switch ev.(type) {
	case *events.NodeShutdownCompleteMessage:
		msg, _ := ev.(*events.NodeShutdownCompleteMessage)
		// Now remove myself from the worker dispatch list. When the anax process terminates,
		// the socket listener will terminate also. This is done on a separate thread so that
		// the message dispatcher doesnt get blocked. This worker isnt actually a full blown
		// worker and doesnt have a command thread that it can run on.
		switch msg.Event().Id {
		case events.UNCONFIGURE_COMPLETE:
			// This is for the situation where the agbot is running on a node.
			go func() {
				a.Messages() <- events.NewWorkerStopMessage(events.WORKER_STOP, a.GetName())
			}()
		case events.AGBOT_QUIESCE_COMPLETE:
			a.em.RecordEvent(msg, func(m events.Message) { a.saveShutdownError(m) })
			// This is for the situation where the agbot is running stand alone.
			go func() {
				a.Messages() <- events.NewWorkerStopMessage(events.WORKER_STOP, a.GetName())
			}()
		}

	}

	return
}

func (a *SecureAPI) saveShutdownError(msg events.Message) {
	switch msg.(type) {
	case *events.NodeShutdownCompleteMessage:
		m, _ := msg.(*events.NodeShutdownCompleteMessage)
		a.shutdownError = m.Err()
	}
}

func (a *SecureAPI) GetName() string {
	return a.name
}

// A local implementation of the ExchangeContext interface because the API object is not an anax worker.
func (a *SecureAPI) GetExchangeId() string {
	if a.EC != nil {
		return a.EC.Id
	} else {
		return ""
	}
}

func (a *SecureAPI) GetExchangeToken() string {
	if a.EC != nil {
		return a.EC.Token
	} else {
		return ""
	}
}

func (a *SecureAPI) GetExchangeURL() string {
	if a.EC != nil {
		return a.EC.URL
	} else {
		return ""
	}
}

func (a *SecureAPI) GetCSSURL() string {
	if a.EC != nil {
		return a.EC.CSSURL
	} else {
		return ""
	}
}

func (a *SecureAPI) GetHTTPFactory() *config.HTTPClientFactory {
	if a.EC != nil {
		return a.EC.HTTPFactory
	} else {
		return a.Config.Collaborators.HTTPClientFactory
	}
}

// This function sets up the agbot secure http server
func (a *SecureAPI) listen() {
	glog.Info("Starting AgreementBot SecureAPI server")

	// If there is no ir invalid Agbot config, we will terminate
	apiListenHost := a.Config.AgreementBot.SecureAPIListenHost
	apiListenPort := a.Config.AgreementBot.SecureAPIListenPort
	certFile := a.Config.AgreementBot.SecureAPIServerCert
	keyFile := a.Config.AgreementBot.SecureAPIServerKey
	if apiListenHost == "" {
		glog.Errorf("AgreementBotWorker SecureAPI terminating, no AgreementBot SecureAPIListenHost config.")
		return
	} else if apiListenPort == "" {
		glog.Errorf("AgreementBotWorker SecureAPI terminating, no AgreementBot SecureAPIListenPort config.")
		return
	} else if a.db == nil {
		glog.Errorf("AgreementBotWorker SecureAPI terminating, no AgreementBot database configured.")
		return
	} else if certFile == "" {
		glog.Errorf("AgreementBotWorker SecureAPI terminating, no SecureAPIServerCert config.")
		return
	} else if !fileExists(certFile) {
		glog.Errorf("AgreementBotWorker SecureAPI terminating, secure API server certificate file %v does not exist.", certFile)
		return
	} else if keyFile == "" {
		glog.Errorf("AgreementBotWorker SecureAPI terminating, no SecureAPIServerKey API config.")
		return
	} else if !fileExists(keyFile) {
		glog.Errorf("AgreementBotWorker SecureAPI terminating, secure API server key file %v does not exist.", keyFile)
		return
	}

	glog.V(3).Infof(APIlogString(fmt.Sprintf("Starting AgreementBot SecureAPI server with address: %v:%v, cert file: %v, key file: %v", apiListenHost, apiListenPort, certFile, keyFile)))

	nocache := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Add("Cache-Control", "no-cache, no-store, must-revalidate")
			w.Header().Add("Pragma", "no-cache, no-store")
			w.Header().Add("Access-Control-Allow-Origin", "*")
			w.Header().Add("Access-Control-Allow-Headers", "X-Requested-With, content-type, Authorization")
			w.Header().Add("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, PATCH, OPTIONS")
			h.ServeHTTP(w, r)
		})
	}

	// This routine does not need to be a subworker because it will terminate on its own when the main
	// anax process terminates.
	go func() {
		router := mux.NewRouter()

		router.HandleFunc("/policycompatible", a.policy_compatible).Methods("GET", "OPTIONS")

		apiListen := fmt.Sprintf("%v:%v", apiListenHost, apiListenPort)
		if err := http.ListenAndServeTLS(apiListen, certFile, keyFile, nocache(router)); err != nil {
			glog.Fatalf(APIlogString(fmt.Sprintf("failed to start listener on %v, error %v", apiListen, err)))
		}
	}()
}

// This function does policy compatibility check.
func (a *SecureAPI) policy_compatible(w http.ResponseWriter, r *http.Request) {

	switch r.Method {
	case "GET":
		glog.V(5).Infof(APIlogString(fmt.Sprintf("/policycompatible called.")))

		userId, userPasswd, ok := r.BasicAuth()
		if !ok {
			glog.Errorf(APIlogString(fmt.Sprintf("/policycompatible is called without exchange authentication.")))
			writeResponse(w, "Unauthorized. No exchange user id is supplied.", http.StatusForbidden)
		} else if err := a.authenticateWithExchange(userId, userPasswd); err != nil {
			glog.Errorf(APIlogString(fmt.Sprintf("Failed to authenticate user %v with the exchange. %v", userId, err)))
			writeResponse(w, fmt.Sprintf("Failed to authenticate the user with the exchange. %v", err), http.StatusForbidden)
		} else {
			// write good output
			ret := map[string]string{"status": "everything is ok."}
			writeResponse(w, ret, http.StatusOK)
		}

	case "OPTIONS":
		w.Header().Set("Allow", "GET, OPTIONS")
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// This function checks if file exits or not
func fileExists(filename string) bool {
	fileinfo, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	if fileinfo.IsDir() {
		return false
	}

	return true
}

// This function verifies the given exchange user name and password.
// The user must be in the format of orgId/userId.
func (a *SecureAPI) authenticateWithExchange(user string, userPasswd string) error {
	glog.V(5).Infof(APIlogString(fmt.Sprintf("authenticateWithExchange called with user %v", user)))

	orgId, userId := cutil.SplitOrgSpecUrl(user)
	if userId == "" {
		return fmt.Errorf("No exchange user id is supplied.")
	} else if orgId == "" {
		return fmt.Errorf("No exchange user organization id is supplied.")
	} else if userPasswd == "" {
		return fmt.Errorf("No exchange user password or api key is supplied.")
	}

	// Invoke the exchange API to verify the user.
	retryCount := 2
	for {
		retryCount = retryCount - 1

		var resp interface{}
		resp = new(exchange.GetUsersResponse)
		targetURL := fmt.Sprintf("%vorgs/%v/users/%v", a.GetExchangeURL(), orgId, userId)

		if err, tpErr := exchange.InvokeExchange(a.httpClient, "GET", targetURL, user, userPasswd, nil, &resp); err != nil {
			glog.Errorf(APIlogString(err.Error()))

			if strings.Contains(err.Error(), "401") {
				return fmt.Errorf("User not found.")
			} else {
				return err
			}
		} else if tpErr != nil {
			glog.Warningf(APIlogString(tpErr.Error()))

			if retryCount == 0 {
				return tpErr
			}
			time.Sleep(10 * time.Second)
			continue
		} else {
			return nil
		}
	}

	return nil
}