/*
Copyright 2017 Mirantis

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tapmanager

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/containernetworking/cni/pkg/ns"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/davecgh/go-spew/spew"
	"github.com/golang/glog"

	"github.com/Mirantis/virtlet/pkg/cni"
	"github.com/Mirantis/virtlet/pkg/dhcp"
	"github.com/Mirantis/virtlet/pkg/nettools"
)

type PodNetworkDesc struct {
	PodId   string `json:"podId"`
	PodNs   string `json:"podNs"`
	PodName string `json:"podName"`
	DNS     *cnitypes.DNS
}

type podNetwork struct {
	pnd        PodNetworkDesc
	csn        *nettools.ContainerSideNetwork
	dhcpServer *dhcp.Server
	doneCh     chan error
}

// TODO: add test & doc
type TapFDSource struct {
	cniClient *cni.Client
	fdMap     map[string]*podNetwork
}

var _ FDSource = &TapFDSource{}

func NewTapFDSource(cniPluginsDir, cniConfigsDir string) (*TapFDSource, error) {
	cniClient, err := cni.NewClient(cniPluginsDir, cniConfigsDir)
	if err != nil {
		return nil, err
	}

	return &TapFDSource{
		cniClient: cniClient,
		fdMap:     make(map[string]*podNetwork),
	}, nil
}

func (s *TapFDSource) GetFD(key string, data []byte) (int, []byte, error) {
	var pnd PodNetworkDesc
	if err := json.Unmarshal(data, &pnd); err != nil {
		return 0, nil, fmt.Errorf("error unmarshalling pod network desc: %v", err)
	}

	if err := cni.CreateNetNS(pnd.PodId); err != nil {
		return 0, nil, fmt.Errorf("error creating new netns for pod %s (%s): %v", pnd.PodName, pnd.PodId, err)
	}

	netConfig, err := s.cniClient.AddSandboxToNetwork(pnd.PodId, pnd.PodName, pnd.PodNs)
	if err != nil {
		return 0, nil, fmt.Errorf("error adding pod %s (%s) to CNI network: %v", pnd.PodName, pnd.PodId, err)
	}
	glog.V(3).Infof("CNI configuration for pod %s (%s): %s", pnd.PodName, pnd.PodId, spew.Sdump(netConfig))

	if pnd.DNS != nil {
		netConfig.DNS.Nameservers = pnd.DNS.Nameservers
		netConfig.DNS.Search = pnd.DNS.Search
		netConfig.DNS.Options = pnd.DNS.Options
	}

	netNSPath := cni.PodNetNSPath(pnd.PodId)

	vmNS, err := ns.GetNS(netNSPath)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to open network namespace at %q: %v", netNSPath, err)
	}

	var csn *nettools.ContainerSideNetwork
	var dhcpServer *dhcp.Server
	doneCh := make(chan error)
	if err := vmNS.Do(func(ns.NetNS) error {
		var err error
		csn, err = nettools.SetupContainerSideNetwork(netConfig)
		if err != nil {
			return err
		}
		dhcpConfg := &dhcp.Config{
			CNIResult:           *csn.Result,
			PeerHardwareAddress: csn.HardwareAddr,
		}
		dhcpServer = dhcp.NewServer(dhcpConfg)
		if err := dhcpServer.SetupListener("0.0.0.0"); err != nil {
			return fmt.Errorf("Failed to set up dhcp listener: %v", err)
		}
		go func() {
			doneCh <- vmNS.Do(func(ns.NetNS) error {
				err := dhcpServer.Serve()
				if err != nil {
					glog.Errorf("dhcp server error: %v", err)
				}
				return err
			})
		}()
		// FIXME: there's some very small possibility for a race here
		// (happens if the VM makes DHCP request before DHCP server is ready)
		// For now, let's make the probability of such problem even smaller
		time.Sleep(500 * time.Millisecond)
		return nil
	}); err != nil {
		return 0, nil, err
	}

	respData, err := json.Marshal(netConfig)
	if err != nil {
		return 0, nil, fmt.Errorf("error marshalling net config: %v", err)
	}

	s.fdMap[key] = &podNetwork{
		pnd:        pnd,
		csn:        csn,
		dhcpServer: dhcpServer,
		doneCh:     doneCh,
	}
	return int(csn.TapFile.Fd()), respData, nil
}

func (s *TapFDSource) Release(key string) error {
	pn, found := s.fdMap[key]
	if !found {
		return fmt.Errorf("bad fd key: %q", key)
	}

	netNSPath := cni.PodNetNSPath(pn.pnd.PodId)

	vmNS, err := ns.GetNS(netNSPath)
	if err != nil {
		return fmt.Errorf("failed to open network namespace at %q: %v", netNSPath, err)
	}

	if err := vmNS.Do(func(ns.NetNS) error {
		if err := pn.dhcpServer.Close(); err != nil {
			return fmt.Errorf("failed to stop dhcp server: %v", err)
		}
		<-pn.doneCh
		if err := pn.csn.Teardown(); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}

	if err := s.cniClient.RemoveSandboxFromNetwork(pn.pnd.PodId, pn.pnd.PodName, pn.pnd.PodNs); err != nil {
		return fmt.Errorf("error removing pod sandbox %q from CNI network: %v", pn.pnd.PodId, err)
	}

	if err := cni.DestroyNetNS(pn.pnd.PodId); err != nil {
		return fmt.Errorf("Error when removing network namespace for pod sandbox %q: %v", pn.pnd.PodId, err)
	}

	delete(s.fdMap, key)
	return nil
}

func (s *TapFDSource) GetInfo(key string) ([]byte, error) {
	pn, found := s.fdMap[key]
	if !found {
		return nil, fmt.Errorf("bad fd key: %q", key)
	}
	return pn.csn.HardwareAddr, nil
}
