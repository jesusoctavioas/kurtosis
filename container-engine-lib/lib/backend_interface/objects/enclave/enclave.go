package enclave

import (
	"github.com/kurtosis-tech/free-ip-addr-tracker-lib/lib"
	"net"
)

type EnclaveID string

type Enclave struct {
	id EnclaveID
	status EnclaveStatus
	//TODO REMOVE THIS WHEN WE ALL DOCKER LOGIC IS ABSTRACTED IN THE BACKEND
	networkID string
	//TODO REMOVE THIS WHEN WE ALL DOCKER LOGIC IS ABSTRACTED IN THE BACKEND
	networkCIDR string
	//TODO REMOVE THIS WHEN WE ALL DOCKER LOGIC IS ABSTRACTED IN THE BACKEND
	networkGatewayIp net.IP
	//TODO REMOVE THIS WHEN WE ALL DOCKER LOGIC IS ABSTRACTED IN THE BACKEND
	networkIpAddrTracker *lib.FreeIpAddrTracker
}

func NewEnclave(id EnclaveID, status EnclaveStatus, networkID string, networkCIDR string, networkGatewayIp net.IP, networkIpAddrTracker *lib.FreeIpAddrTracker) *Enclave {
	return &Enclave{id: id, status: status, networkID: networkID, networkCIDR: networkCIDR, networkGatewayIp: networkGatewayIp, networkIpAddrTracker: networkIpAddrTracker}
}

func (enclave *Enclave) GetID() EnclaveID {
	return enclave.id
}

func (enclave *Enclave) GetStatus() EnclaveStatus {
	return enclave.status
}

func (enclave *Enclave) GetNetworkID() string {
	return enclave.networkID
}

func (enclave *Enclave) GetNetworkCIDR() string {
	return enclave.networkCIDR
}

func (enclave *Enclave) GetNetworkGatewayIp() net.IP {
	return enclave.networkGatewayIp
}

func (enclave *Enclave) GetNetworkIpAddrTracker() *lib.FreeIpAddrTracker {
	return enclave.networkIpAddrTracker
}
