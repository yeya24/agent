package ha

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/cortexproject/cortex/pkg/ring"
	"github.com/cortexproject/cortex/pkg/ring/kv"
	"github.com/cortexproject/cortex/pkg/ring/kv/consul"
	"github.com/cortexproject/cortex/pkg/util/flagext"
	"github.com/go-kit/kit/log"
	"github.com/gorilla/mux"
	"github.com/grafana/agent/pkg/client"
	haClient "github.com/grafana/agent/pkg/prom/ha/client"
	"github.com/grafana/agent/pkg/prom/ha/configapi"
	"github.com/grafana/agent/pkg/prom/instance"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/config"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"
)

func TestServer_ListConfigurations(t *testing.T) {
	env := newAPITestEnvironment(t)

	// Store some configs
	cfgs := []*instance.Config{
		{Name: "a"},
		{Name: "b"},
		{Name: "c"},
	}
	for _, cfg := range cfgs {
		err := env.ha.kv.CAS(context.Background(), cfg.Name, func(in interface{}) (out interface{}, retry bool, err error) {
			return cfg, false, nil
		})
		require.NoError(t, err)
	}

	resp, err := http.Get(env.srv.URL + "/agent/api/v1/configs")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var apiResp configapi.ListConfigurationsResponse
	unmarshalTestResponse(t, resp.Body, &apiResp)
	sort.Strings(apiResp.Configs)

	expect := configapi.ListConfigurationsResponse{Configs: []string{"a", "b", "c"}}
	require.Equal(t, expect, apiResp)

	t.Run("With Client", func(t *testing.T) {
		cli := client.New(env.srv.URL)
		apiResp, err := cli.ListConfigs(context.Background())
		require.NoError(t, err)

		sort.Strings(apiResp.Configs)

		expect := &configapi.ListConfigurationsResponse{Configs: []string{"a", "b", "c"}}
		require.Equal(t, expect, apiResp)
	})
}

// TestServer_GetConfiguration_Invalid makes sure that requesting an invalid
// config does not panic.
func TestServer_GetConfiguration_Invalid(t *testing.T) {
	env := newAPITestEnvironment(t)

	resp, err := http.Get(env.srv.URL + "/agent/api/v1/configs/does-not-exist")
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	var apiResp configapi.ErrorResponse
	unmarshalTestResponse(t, resp.Body, &apiResp)
	require.Equal(t, "configuration does-not-exist does not exist", apiResp.Error)

	t.Run("With Client", func(t *testing.T) {
		cli := client.New(env.srv.URL)
		_, err := cli.GetConfiguration(context.Background(), "does-not-exist")
		require.NotNil(t, err)
		require.Equal(t, "configuration does-not-exist does not exist", err.Error())
	})
}

func TestServer_GetConfiguration(t *testing.T) {
	env := newAPITestEnvironment(t)

	cfg := instance.DefaultConfig
	cfg.Name = "a"
	cfg.HostFilter = true
	cfg.RemoteFlushDeadline = 10 * time.Minute
	err := env.ha.kv.CAS(context.Background(), cfg.Name, func(in interface{}) (out interface{}, retry bool, err error) {
		return &cfg, false, nil
	})
	require.NoError(t, err)

	resp, err := http.Get(env.srv.URL + "/agent/api/v1/configs/a")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var apiResp configapi.GetConfigurationResponse
	unmarshalTestResponse(t, resp.Body, &apiResp)

	var actual instance.Config
	err = yaml.Unmarshal([]byte(apiResp.Value), &actual)
	require.NoError(t, err)

	require.Equal(t, cfg, actual, "unmarshaled stored configuration did not match input")

	t.Run("With Client", func(t *testing.T) {
		cli := client.New(env.srv.URL)
		actual, err := cli.GetConfiguration(context.Background(), "a")
		require.NoError(t, err)
		require.Equal(t, &cfg, actual)
	})
}

func TestServer_PutConfiguration(t *testing.T) {
	env := newAPITestEnvironment(t)

	cfg := instance.DefaultConfig
	cfg.Name = "newconfig"
	cfg.HostFilter = false
	cfg.RemoteFlushDeadline = 10 * time.Minute
	cfg.ScrapeConfigs = []*config.ScrapeConfig{{
		JobName:     "test_job",
		MetricsPath: "/metrics",
		Scheme:      "http",
	}}
	_ = cfg.ApplyDefaults(&config.DefaultGlobalConfig)

	bb, err := yaml.Marshal(cfg)
	require.NoError(t, err)

	// Try calling Put twice; first time should create the config and the second
	// should update it.
	expectedResponses := []int{http.StatusCreated, http.StatusOK}
	for _, expectedResp := range expectedResponses {
		resp, err := http.Post(env.srv.URL+"/agent/api/v1/config/newconfig", "", bytes.NewReader(bb))
		require.NoError(t, err)
		require.Equal(t, expectedResp, resp.StatusCode)
		unmarshalTestResponse(t, resp.Body, nil)

		// Get the stored config back
		resp, err = http.Get(env.srv.URL + "/agent/api/v1/configs/newconfig")
		require.NoError(t, err)
		var apiResp configapi.GetConfigurationResponse
		unmarshalTestResponse(t, resp.Body, &apiResp)

		var actual instance.Config
		err = yaml.Unmarshal([]byte(apiResp.Value), &actual)
		require.NoError(t, err)
		require.Equal(t, cfg, actual, "unmarshaled stored configuration did not match input")
	}
}

func TestServer_PutConfiguration_URLEncoded(t *testing.T) {
	env := newAPITestEnvironment(t)

	cfg := instance.DefaultConfig
	cfg.Name = "url/encoded"
	cfg.HostFilter = false
	cfg.RemoteFlushDeadline = 10 * time.Minute
	cfg.ScrapeConfigs = []*config.ScrapeConfig{{
		JobName:     "encoded_test_job",
		MetricsPath: "/metrics",
		Scheme:      "http",
	}}
	_ = cfg.ApplyDefaults(&config.DefaultGlobalConfig)

	bb, err := yaml.Marshal(cfg)
	require.NoError(t, err)

	// Try calling Put twice; first time should create the config and the second
	// should update it.
	expectedResponses := []int{http.StatusCreated, http.StatusOK}
	for _, expectedResp := range expectedResponses {
		resp, err := http.Post(env.srv.URL+"/agent/api/v1/config/url%2Fencoded", "", bytes.NewReader(bb))
		require.NoError(t, err)
		unmarshalTestResponse(t, resp.Body, nil)
		require.Equal(t, expectedResp, resp.StatusCode)

		// Get the stored config back
		resp, err = http.Get(env.srv.URL + "/agent/api/v1/configs/url%2Fencoded")
		require.NoError(t, err)
		var apiResp configapi.GetConfigurationResponse
		unmarshalTestResponse(t, resp.Body, &apiResp)

		var actual instance.Config
		err = yaml.Unmarshal([]byte(apiResp.Value), &actual)
		require.NoError(t, err)
		require.Equal(t, cfg, actual, "unmarshaled stored configuration did not match input")
	}
}

func TestServer_PutConfiguration_NonUnique(t *testing.T) {
	env := newAPITestEnvironment(t)

	conflictA := instance.DefaultConfig
	conflictA.Name = "conflict-a"
	conflictA.HostFilter = false
	conflictA.RemoteFlushDeadline = 10 * time.Minute
	conflictA.ScrapeConfigs = []*config.ScrapeConfig{{
		JobName:     "conflicting",
		MetricsPath: "/metrics",
		Scheme:      "http",
	}}
	_ = conflictA.ApplyDefaults(&config.DefaultGlobalConfig)

	//
	// Put conflict A; it should succeed
	//
	bb, err := yaml.Marshal(conflictA)
	require.NoError(t, err)

	resp, err := http.Post(env.srv.URL+"/agent/api/v1/config/conflict-a", "", bytes.NewReader(bb))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	unmarshalTestResponse(t, resp.Body, nil)

	//
	// Put conflict B; it should fail because conflict-a already
	// has a job with the name "conflicting."
	//
	conflictB := conflictA
	conflictB.Name = "conflict-b"

	bb, err = yaml.Marshal(conflictB)
	require.NoError(t, err)

	resp, err = http.Post(env.srv.URL+"/agent/api/v1/config/conflict-b", "", bytes.NewReader(bb))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	var errResp configapi.ErrorResponse
	unmarshalTestResponse(t, resp.Body, &errResp)
	require.Equal(t, `found multiple scrape configs with job name "conflicting"`, errResp.Error)
}

func TestServer_PutConfiguration_Invalid(t *testing.T) {
	env := newAPITestEnvironment(t)

	cfg := instance.DefaultConfig
	cfg.Name = "newconfig"
	cfg.ScrapeConfigs = []*config.ScrapeConfig{nil}

	cli := client.New(env.srv.URL)
	err := cli.PutConfiguration(context.Background(), "newconfig-invalid", &cfg)
	require.EqualError(t, err, "empty or null scrape config section")
}

func TestServer_PutConfiguration_WithClient(t *testing.T) {
	env := newAPITestEnvironment(t)

	cfg := instance.DefaultConfig
	cfg.Name = "newconfig-withclient"
	cfg.HostFilter = true
	cfg.RemoteFlushDeadline = 10 * time.Minute

	cli := client.New(env.srv.URL)
	err := cli.PutConfiguration(context.Background(), "newconfig-withclient", &cfg)
	require.NoError(t, err)

	// Get the config back now
	resp, err := cli.GetConfiguration(context.Background(), "newconfig-withclient")
	require.NoError(t, err)
	require.Equal(t, &cfg, resp, "stored configuration did not match input")
}

func TestServer_DeleteConfiguration(t *testing.T) {
	env := newAPITestEnvironment(t)

	// Store some configs
	cfgs := []*instance.Config{
		{Name: "a"},
		{Name: "b"},
		{Name: "c"},
	}
	for _, cfg := range cfgs {
		err := env.ha.kv.CAS(context.Background(), cfg.Name, func(in interface{}) (out interface{}, retry bool, err error) {
			return cfg, false, nil
		})
		require.NoError(t, err)
	}

	// Delete the configs
	for _, cfg := range cfgs {
		req, err := http.NewRequest("DELETE", env.srv.URL+"/agent/api/v1/config/"+cfg.Name, nil)
		require.NoError(t, err)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		unmarshalTestResponse(t, resp.Body, nil)
	}

	// Do a list, nothing we stored should be there anymore.
	resp, err := http.Get(env.srv.URL + "/agent/api/v1/configs")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var apiResp configapi.ListConfigurationsResponse
	unmarshalTestResponse(t, resp.Body, &apiResp)
	for _, cfg := range cfgs {
		require.NotContains(t, apiResp.Configs, cfg.Name)
	}
}

func TestServer_DeleteConfiguration_WithClient(t *testing.T) {
	env := newAPITestEnvironment(t)

	// Store some configs
	cfgs := []*instance.Config{
		{Name: "a"},
		{Name: "b"},
		{Name: "c"},
	}
	for _, cfg := range cfgs {
		err := env.ha.kv.CAS(context.Background(), cfg.Name, func(in interface{}) (out interface{}, retry bool, err error) {
			return cfg, false, nil
		})
		require.NoError(t, err)
	}

	cli := client.New(env.srv.URL)

	// Delete the configs
	for _, cfg := range cfgs {
		err := cli.DeleteConfiguration(context.Background(), cfg.Name)
		require.NoError(t, err)
	}

	resp, err := cli.ListConfigs(context.Background())
	require.NoError(t, err)
	for _, cfg := range cfgs {
		require.NotContains(t, resp.Configs, cfg.Name)
	}
}

type apiTestEnvironment struct {
	ha     *Server
	srv    *httptest.Server
	router *mux.Router
}

func newAPITestEnvironment(t *testing.T) apiTestEnvironment {
	t.Helper()

	dir, err := ioutil.TempDir(os.TempDir(), "etcd_backend_test")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })

	router := mux.NewRouter()
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	// Create a new HA service with an HTTP store
	logger := log.NewLogfmtLogger(log.NewSyncWriter(os.Stderr))
	ha, err := New(prometheus.NewRegistry(), Config{
		Enabled:         true,
		ReshardInterval: time.Minute * 999,
		KVStore: kv.Config{
			Store:  "inmemory",
			Prefix: "configs/",
		},
		Lifecycler: testLifecyclerConfig(),
	}, &config.DefaultGlobalConfig, haClient.Config{}, logger, newFakeInstanceManager())
	require.NoError(t, err)

	// Wire the API
	ha.WireAPI(router)

	return apiTestEnvironment{
		ha:     ha,
		srv:    srv,
		router: router,
	}
}

func testLifecyclerConfig() ring.LifecyclerConfig {
	var cfg ring.LifecyclerConfig
	flagext.DefaultValues(&cfg)
	cfg.NumTokens = 1
	cfg.ListenPort = 0
	cfg.Addr = "localhost"
	cfg.ID = "localhost"
	cfg.FinalSleep = 0

	inmemoryKV := consul.NewInMemoryClient(ring.GetCodec())
	cfg.RingConfig.ReplicationFactor = 1
	cfg.RingConfig.KVStore.Mock = inmemoryKV
	return cfg
}

// unmarshalTestResponse will unmarshal a test response's data to v. If v is
// nil, unmarshalTestResponse expects that the test response's data should be
// empty.
func unmarshalTestResponse(t *testing.T, r io.ReadCloser, v interface{}) {
	defer r.Close()
	t.Helper()

	resp := struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
	}{}

	err := json.NewDecoder(r).Decode(&resp)
	require.NoError(t, err)

	if v == nil {
		require.True(t, len(resp.Data) == 0, "data in response was not empty as expected. Got %s", resp.Data)
		return
	}

	err = json.Unmarshal(resp.Data, v)
	require.NoError(t, err)
}

func newFakeInstanceManager() instance.Manager {
	var mut sync.Mutex
	var cfgs = make(map[string]instance.Config)

	return &instance.MockManager{
		// ListInstances isn't used in this package, so we won't bother to try to
		// fake it here.
		ListInstancesFunc: func() map[string]instance.ManagedInstance { return nil },

		ListConfigsFunc: func() map[string]instance.Config {
			mut.Lock()
			defer mut.Unlock()

			cp := make(map[string]instance.Config, len(cfgs))
			for k, v := range cfgs {
				cp[k] = v
			}
			return cp
		},

		ApplyConfigFunc: func(c instance.Config) error {
			mut.Lock()
			defer mut.Unlock()
			cfgs[c.Name] = c
			return nil
		},

		DeleteConfigFunc: func(name string) error {
			mut.Lock()
			defer mut.Unlock()
			delete(cfgs, name)
			return nil
		},

		StopFunc: func() {},
	}
}
