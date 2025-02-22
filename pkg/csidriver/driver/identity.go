/*
   Copyright @ 2021 bocloud <fushaosong@beyondcent.com>.

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
package driver

import (
	"context"
	"github.com/carina-io/carina/pkg/csidriver/csi"
	"github.com/carina-io/carina/utils"
	"github.com/carina-io/carina/utils/log"
	"github.com/golang/protobuf/ptypes/wrappers"
)

// NewIdentityService returns a new IdentityServer.

func NewIdentityService() csi.IdentityServer {
	return &identityService{}
}

type identityService struct {
	csi.UnimplementedIdentityServer
}

func (s identityService) GetPluginInfo(ctx context.Context, req *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	log.Info("GetPluginInfo req ", req.String())
	return &csi.GetPluginInfoResponse{
		Name:          utils.CSIPluginName,
		VendorVersion: utils.Version,
	}, nil
}

func (s identityService) GetPluginCapabilities(ctx context.Context, req *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	log.Info("GetPluginCapabilities req ", req.String())
	return &csi.GetPluginCapabilitiesResponse{
		Capabilities: []*csi.PluginCapability{
			{
				Type: &csi.PluginCapability_Service_{
					Service: &csi.PluginCapability_Service{
						Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
					},
				},
			},
			{
				Type: &csi.PluginCapability_Service_{
					Service: &csi.PluginCapability_Service{
						Type: csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS,
					},
				},
			},
			{
				Type: &csi.PluginCapability_VolumeExpansion_{
					VolumeExpansion: &csi.PluginCapability_VolumeExpansion{
						Type: csi.PluginCapability_VolumeExpansion_ONLINE,
					},
				},
			},
			{
				Type: &csi.PluginCapability_VolumeExpansion_{
					VolumeExpansion: &csi.PluginCapability_VolumeExpansion{
						Type: csi.PluginCapability_VolumeExpansion_OFFLINE,
					},
				},
			},
		},
	}, nil
}

func (s identityService) Probe(ctx context.Context, req *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	log.Info("Probe req ", req.String())
	return &csi.ProbeResponse{Ready: &wrappers.BoolValue{Value: true}}, nil
}
