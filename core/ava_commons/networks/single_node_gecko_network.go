package networks

import (
	"github.com/gmarchetti/kurtosis/ava_commons/services"
	"github.com/gmarchetti/kurtosis/commons/testnet"
	"github.com/palantir/stacktrace"
)

type SingleNodeGeckoNetwork struct{
	rawNetwork testnet.RawServiceNetwork
}
func (network SingleNodeGeckoNetwork) GetNode() services.GeckoService {
	return network.rawNetwork.Services[0].(services.GeckoService)
}

type SingleNodeGeckoNetworkLoader struct {}
func (loader SingleNodeGeckoNetworkLoader) GetNetworkConfig(testImageName string) (*testnet.ServiceNetworkConfig, error) {
	factoryConfig := services.NewGeckoServiceFactoryConfig(
		testImageName,
		1,
		1,
		false,
		services.LOG_LEVEL_DEBUG)
	factory := testnet.NewServiceFactory(factoryConfig)

	builder := testnet.NewServiceNetworkConfigBuilder()
	config1 := builder.AddServiceConfiguration(*factory)
	_, err := builder.AddService(config1, make(map[int]bool))
	if err != nil {
		return nil, stacktrace.Propagate(err, "Could not add service")
	}
	return builder.Build(), nil
}
func (loader SingleNodeGeckoNetworkLoader) LoadNetwork(network testnet.RawServiceNetwork) (interface{}, error) {
	return SingleNodeGeckoNetwork{rawNetwork: network}, nil
}
