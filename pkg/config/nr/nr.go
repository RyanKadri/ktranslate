package nr

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/Khan/genqlient/graphql"
	"github.com/kentik/ktranslate"
	"github.com/kentik/ktranslate/pkg/eggs/logger"
	"github.com/kentik/ktranslate/pkg/generated"
	snmp_util "github.com/kentik/ktranslate/pkg/inputs/snmp/util"
	"github.com/kentik/ktranslate/pkg/kt"
	"gopkg.in/yaml.v3"
)

const (
	EnvNrApiKey = "NEW_RELIC_USER_KEY"
)

type NRConfig struct {
	logger.ContextL
	currentConfig *ktranslate.Config
	configClient  graphql.Client
	lastRevision  int
	agentId       string
}

type authedTransport struct {
	wrapped http.RoundTripper
}

func (t *authedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("Api-Key", os.Getenv(EnvNrApiKey))
	return t.wrapped.RoundTrip(req)
}

func NewConfig(log logger.Underlying, cfg *ktranslate.Config) (*NRConfig, error) {
	gqlClient := graphql.NewClient("https://localhost.newrelic.com:3100/graphql", &http.Client{Transport: &authedTransport{wrapped: http.DefaultTransport}})
	nr := NRConfig{
		ContextL:      logger.NewContextLFromUnderlying(logger.SContext{S: "nrConfig"}, log),
		currentConfig: cfg,
		configClient:  gqlClient,
		lastRevision:  -1,
	}

	nr.getConfig(context.TODO())

	return &nr, nil
}

func (nr *NRConfig) Run(ctx context.Context, cb func(*ktranslate.Config) error) {
	checkTicker := time.NewTicker(time.Second * time.Duration(nr.currentConfig.CfgManager.PollTimeSec))
	defer checkTicker.Stop()

	nr.Infof("config checker running")
	for {
		select {
		case <-checkTicker.C:
			nr.updateConfig(ctx, cb)
		case <-ctx.Done():
			nr.Infof("config checker done")
			return
		}
	}
}

func (nr *NRConfig) updateConfig(ctx context.Context, cb func(*ktranslate.Config) error) {
	// Get config
	newConfig, newVersion, err := nr.getConfig(ctx)
	if err != nil {
		nr.Errorf("Cannot load new config: %v", err)
	}

	if newConfig != nil && newVersion {
		nr.currentConfig = newConfig
		err := cb(newConfig)
		if err != nil {
			nr.Errorf("Cannot update to new config: %v", err)
		}
	}
}

func (nr *NRConfig) DeviceDiscovery(config *kt.SnmpConfig) {
	output, _ := yaml.Marshal(config)
	generated.PublishNewConfig(context.TODO(), nr.configClient, nr.agentId, string(output))
}

func (nr *NRConfig) Close() {

}

func (nr *NRConfig) getConfig(ctx context.Context) (*ktranslate.Config, bool, error) {
	acctId, _ := strconv.Atoi(nr.currentConfig.NewRelicSink.Account)
	configResponse, err := generated.FetchCollectorConfiguration(ctx, nr.configClient, nr.currentConfig.Server.ServiceName, acctId)
	if err != nil {
		return nil, false, err
	}
	fetchedConfig := configResponse.Actor.NetworkMonitoring.AgentConfigurations[0]
	nr.agentId = fetchedConfig.Id
	configBytes := []byte(fetchedConfig.RawConfiguration)
	snmpConfig := kt.SnmpConfig{}
	err = yaml.Unmarshal(configBytes, &snmpConfig)
	if err != nil {
		return nil, false, err
	}

	// Save out the config file.
	t, err := yaml.Marshal(snmpConfig)
	if err != nil {
		return nil, false, err
	}

	snmp_util.WriteFile(ctx, nr.currentConfig.SNMPInput.SNMPFile, t, 0644)

	if fetchedConfig.RevisionId != nr.lastRevision {
		nr.Infof("New config detected")
		nr.lastRevision = fetchedConfig.RevisionId
		return nr.currentConfig, true, nil
	}
	return nil, false, nil
}
