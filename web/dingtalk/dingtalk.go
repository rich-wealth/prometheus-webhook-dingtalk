package dingtalk

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"

	"github.com/timonwong/prometheus-webhook-dingtalk/config"
	"github.com/timonwong/prometheus-webhook-dingtalk/notifier"
	"github.com/timonwong/prometheus-webhook-dingtalk/pkg/chilog"
	"github.com/timonwong/prometheus-webhook-dingtalk/pkg/models"
	"github.com/timonwong/prometheus-webhook-dingtalk/template"
)

type API struct {
	// Protect against config, template and http client
	mtx sync.RWMutex

	conf       *config.Config
	tmpl       *template.Template
	targets    map[string]config.Target
	httpClient *http.Client
	logger     log.Logger
}

func NewAPI(logger log.Logger) *API {
	return &API{
		logger: logger,
	}
}

func (api *API) Update(conf *config.Config, tmpl *template.Template) {
	api.mtx.Lock()
	defer api.mtx.Unlock()

	api.conf = conf
	api.tmpl = tmpl
	api.targets = conf.Targets
	api.httpClient = &http.Client{
		Transport: &http.Transport{
			Proxy:             http.ProxyFromEnvironment,
			DisableKeepAlives: true,
		},
	}
}

func (api *API) Routes() chi.Router {
	router := chi.NewRouter()
	router.Use(middleware.RealIP)
	router.Use(middleware.RequestLogger(&chilog.KitLogger{Logger: api.logger}))
	router.Use(middleware.Recoverer)
	router.Post("/{name}/send", api.serveSend)
	return router
}

func (api *API) serveSend(w http.ResponseWriter, r *http.Request) {
	api.mtx.RLock()
	targets := api.targets
	conf := api.conf
	tmpl := api.tmpl
	httpClient := api.httpClient
	api.mtx.RUnlock()

	targetName := chi.URLParam(r, "name")
	logger := log.With(api.logger, "target", targetName)

	target, ok := targets[targetName]
	if !ok {
		level.Warn(logger).Log("msg", "target not found")
		http.NotFound(w, r)
		return
	}

	var promMessage models.WebhookMessage
	if err := json.NewDecoder(r.Body).Decode(&promMessage); err != nil {
		level.Error(logger).Log("msg", "Cannot decode prometheus webhook JSON request", "err", err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	builder := notifier.NewDingNotificationBuilder(tmpl, conf, &target)
	notification, err := builder.Build(&promMessage)
	if err != nil {
		level.Error(logger).Log("msg", "Failed to build notification", "err", err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	//send custom third api
	thirdApiUrl := os.Getenv("HTTP_THIRD_API_URL")
	if thirdApiUrl != "" {
		promMessage.Source = os.Getenv("HTTP_THIRD_API_SOURCE")
		promMessage.DingtalkWebhookUrl = target.URL.String()
		_, err := sendThirdApi(&promMessage, thirdApiUrl)
		if err != nil {
			level.Error(logger).Log("msg", "Failed to send third api", "err", err)
		}
	}

	robotResp, err := notifier.SendNotification(notification, httpClient, &target)
	if err != nil {
		level.Error(logger).Log("msg", "Failed to send notification", "err", err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	if robotResp.ErrorCode != 0 {
		level.Error(logger).Log("msg", "Failed to send notification to DingTalk", "respCode", robotResp.ErrorCode, "respMsg", robotResp.ErrorMessage)
		http.Error(w, "Unable to talk to DingTalk", http.StatusBadRequest)
		return
	}

	io.WriteString(w, "OK")
}

func sendThirdApi(promMessage *models.WebhookMessage, url string) (bool, error) {
	if url == "" {
		return false, fmt.Errorf("error, url is empty")
	}
	body, err := json.Marshal(&promMessage)
	if err != nil {
		return false, fmt.Errorf("error encoding prometheus webhook msg: %w", err)
	}

	httpReq, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("error building third api request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpClient := &http.Client{
		Transport: &http.Transport{
			Proxy:             http.ProxyFromEnvironment,
			DisableKeepAlives: true,
		},
	}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return false, fmt.Errorf("error sending third api: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	if resp.StatusCode != 200 {
		return false, fmt.Errorf("unacceptable response code %d", resp.StatusCode)
	}
	return true, nil
}
