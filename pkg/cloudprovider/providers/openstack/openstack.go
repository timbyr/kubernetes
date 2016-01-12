/*
Copyright 2014 The Kubernetes Authors All rights reserved.

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

package openstack

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/rackspace/gophercloud"
	"github.com/rackspace/gophercloud/openstack"
	"github.com/rackspace/gophercloud/openstack/compute/v2/servers"
	"github.com/rackspace/gophercloud/pagination"
	"github.com/scalingdata/gcfg"

	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/cloudprovider"
)

const ProviderName = "openstack"

const metadataUrl = "http://169.254.169.254/openstack/2012-08-10/meta_data.json"

var ErrNotFound = errors.New("Failed to find object")
var ErrMultipleResults = errors.New("Multiple results where only one expected")
var ErrNoAddressFound = errors.New("No address found for host")
var ErrAttrNotFound = errors.New("Expected attribute not found")

// encoding.TextUnmarshaler interface for time.Duration
type MyDuration struct {
	time.Duration
}

func (d *MyDuration) UnmarshalText(text []byte) error {
	res, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	d.Duration = res
	return nil
}

type RouteOpts struct {
	RouterId         string `gcfg:"router-id"` // required
	HostnameOverride bool   `gcfg:"hostname-override"`
}

type LoadBalancerOpts struct {
	SubnetId          string     `gcfg:"subnet-id"` // required
	FloatingNetworkId string     `gcfg:"floating-network-id"`
	LBMethod          string     `gfcg:"lb-method"`
	CreateMonitor     bool       `gcfg:"create-monitor"`
	MonitorDelay      MyDuration `gcfg:"monitor-delay"`
	MonitorTimeout    MyDuration `gcfg:"monitor-timeout"`
	MonitorMaxRetries uint       `gcfg:"monitor-max-retries"`
}

// OpenStack is an implementation of cloud provider Interface for OpenStack.
type OpenStack struct {
	provider  *gophercloud.ProviderClient
	compute   *gophercloud.ServiceClient
	network   *gophercloud.ServiceClient
	region    string
	lbOpts    LoadBalancerOpts
	routeOpts RouteOpts
	// InstanceID of the server where this OpenStack object is instantiated.
	localInstanceID string
}

type Config struct {
	Global struct {
		AuthUrl    string `gcfg:"auth-url"`
		Username   string
		UserId     string `gcfg:"user-id"`
		Password   string
		ApiKey     string `gcfg:"api-key"`
		TenantId   string `gcfg:"tenant-id"`
		TenantName string `gcfg:"tenant-name"`
		DomainId   string `gcfg:"domain-id"`
		DomainName string `gcfg:"domain-name"`
		Region     string
	}
	LoadBalancer LoadBalancerOpts
	Route        RouteOpts
}

func init() {
	cloudprovider.RegisterCloudProvider(ProviderName, func(config io.Reader) (cloudprovider.Interface, error) {
		cfg, err := readConfig(config)
		if err != nil {
			return nil, err
		}
		return newOpenStack(cfg)
	})
}

func (cfg Config) toAuthOptions() gophercloud.AuthOptions {
	return gophercloud.AuthOptions{
		IdentityEndpoint: cfg.Global.AuthUrl,
		Username:         cfg.Global.Username,
		UserID:           cfg.Global.UserId,
		Password:         cfg.Global.Password,
		APIKey:           cfg.Global.ApiKey,
		TenantID:         cfg.Global.TenantId,
		TenantName:       cfg.Global.TenantName,

		// Persistent service, so we need to be able to renew tokens.
		AllowReauth: true,
	}
}

func readConfig(config io.Reader) (Config, error) {
	if config == nil {
		err := fmt.Errorf("no OpenStack cloud provider config file given")
		return Config{}, err
	}

	var cfg Config
	err := gcfg.ReadInto(&cfg, config)
	return cfg, err
}

func newOpenStack(cfg Config) (*OpenStack, error) {
	provider, err := openstack.AuthenticatedClient(cfg.toAuthOptions())
	if err != nil {
		return nil, err
	}

	id, err := readInstanceID()
	if err != nil {
		glog.Info("Not running on an OpenStack Instance")
	}

	network, err := openstack.NewNetworkV2(provider, gophercloud.EndpointOpts{
		Region: cfg.Global.Region,
	})
	if err != nil {
		glog.Warningf("Failed to find neutron endpoint: %v", err)
		return nil, err
	}

	compute, err := openstack.NewComputeV2(provider, gophercloud.EndpointOpts{
		Region: cfg.Global.Region,
	})
	if err != nil {
		glog.Warningf("Failed to find compute endpoint: %v", err)
		return nil, err
	}

	os := OpenStack{
		compute:         compute,
		network:         network,
		provider:        provider,
		region:          cfg.Global.Region,
		lbOpts:          cfg.LoadBalancer,
		routeOpts:       cfg.Route,
		localInstanceID: id,
	}
	return &os, nil
}

func readInstanceID() (string, error) {
	// Try to find instance ID on the local filesystem (created by cloud-init)
	const instanceIDFile = "/var/lib/cloud/data/instance-id"
	idBytes, err := ioutil.ReadFile(instanceIDFile)
	if err == nil {
		instanceID := string(idBytes)
		instanceID = strings.TrimSpace(instanceID)
		glog.V(3).Infof("Got instance id from %s: %s", instanceIDFile, instanceID)
		if instanceID != "" {
			return instanceID, nil
		}
		// Fall through with empty instanceID and try metadata server.
	}
	glog.V(5).Infof("Cannot read %s: '%v', trying metadata server", instanceIDFile, err)

	// Try to get JSON from metdata server.
	resp, err := http.Get(metadataUrl)
	if err != nil {
		glog.V(3).Infof("Cannot read %s: %v", metadataUrl, err)
		return "", err
	}

	if resp.StatusCode != 200 {
		err = fmt.Errorf("got unexpected status code when reading metadata from %s: %s", metadataUrl, resp.Status)
		glog.V(3).Infof("%v", err)
		return "", err
	}

	defer resp.Body.Close()
	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		glog.V(3).Infof("Cannot get HTTP response body from %s: %v", metadataUrl, err)
		return "", err
	}
	instanceID, err := parseMetadataUUID(bodyBytes)
	if err != nil {
		glog.V(3).Infof("Cannot parse instance ID from metadata from %s: %v", metadataUrl, err)
		return "", err
	}

	glog.V(3).Infof("Got instance id from %s: %s", metadataUrl, instanceID)
	return instanceID, nil
}

// parseMetadataUUID reads JSON from OpenStack metadata server and parses
// instance ID out of it.
func parseMetadataUUID(jsonData []byte) (string, error) {
	// We should receive an object with { 'uuid': '<uuid>' } and couple of other
	// properties (which we ignore).

	obj := struct{ UUID string }{}
	err := json.Unmarshal(jsonData, &obj)
	if err != nil {
		return "", err
	}

	uuid := obj.UUID
	if uuid == "" {
		err = fmt.Errorf("cannot parse OpenStack metadata, got empty uuid")
		return "", err
	}

	return uuid, nil
}

func findInstances(compute *gophercloud.ServiceClient, name_filter string) ([]string, error) {
	opts := servers.ListOpts{
		Name:   name_filter,
		Status: "ACTIVE",
	}
	pager := servers.List(compute, opts)

	ret := make([]string, 0)
	err := pager.EachPage(func(page pagination.Page) (bool, error) {
		sList, err := servers.ExtractServers(page)
		if err != nil {
			return false, err
		}
		for _, server := range sList {
			ret = append(ret, server.Name)
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}

	glog.V(3).Infof("Found %v instances matching %v: %v",
		len(ret), name_filter, ret)
	return ret, nil
}

func getServerByName(client *gophercloud.ServiceClient, name string) (*servers.Server, error) {
	opts := servers.ListOpts{
		Name:   fmt.Sprintf("^%s$", regexp.QuoteMeta(name)),
		Status: "ACTIVE",
	}
	pager := servers.List(client, opts)

	serverList := make([]servers.Server, 0)

	err := pager.EachPage(func(page pagination.Page) (bool, error) {
		s, err := servers.ExtractServers(page)
		if err != nil {
			return false, err
		}
		serverList = append(serverList, s...)
		if len(serverList) > 1 {
			return false, ErrMultipleResults
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}

	if len(serverList) == 0 {
		return nil, ErrNotFound
	} else if len(serverList) > 1 {
		return nil, ErrMultipleResults
	}

	return &serverList[0], nil
}

func findAddrs(netblob interface{}, iptype string) []string {
	// Run-tiime types for the win :(
	ret := []string{}
	networks, ok := netblob.(map[string]interface{})
	if !ok {
		return ret
	}

	for _, listblob := range networks {
		list, ok := listblob.([]interface{})
		if !ok || len(list) < 1 {
			continue
		}
		props, ok := list[0].(map[string]interface{})
		if !ok {
			continue
		}
		tmp, ok := props["addr"]
		if !ok {
			continue
		}
		addr, ok := tmp.(string)
		if !ok {
			continue
		}
		tmp, ok = props["OS-EXT-IPS:type"]
		if !ok {
			continue
		}
		ipt, ok := tmp.(string)
		if !ok {
			continue
		}
		if ipt == iptype {
			ret = append(ret, addr)
		}
	}
	return ret
}

func getAddressByName(api *gophercloud.ServiceClient, name string) (string, error) {
	srv, err := getServerByName(api, name)
	if err != nil {
		return "", err
	}

	var s string
	if s == "" {
		if tmp := findAddrs(srv.Addresses, "fixed"); len(tmp) >= 1 {
			s = tmp[0]
		}
	}
	if s == "" {
		if tmp := findAddrs(srv.Addresses, "floating"); len(tmp) >= 1 {
			s = tmp[0]
		}
	}
	if s == "" {
		s = srv.AccessIPv4
	}
	if s == "" {
		s = srv.AccessIPv6
	}
	if s == "" {
		return "", ErrNoAddressFound
	}
	return s, nil
}

func (os *OpenStack) Clusters() (cloudprovider.Clusters, bool) {
	return nil, false
}

// ProviderName returns the cloud provider ID.
func (os *OpenStack) ProviderName() string {
	return ProviderName
}

func (os *OpenStack) ScrubDNS(nameservers, searches []string) (nsOut, srchOut []string) {
	return nameservers, searches
}

func isNotFound(err error) bool {
	e, ok := err.(*gophercloud.UnexpectedResponseCodeError)
	return ok && e.Actual == http.StatusNotFound
}

func (os *OpenStack) Zones() (cloudprovider.Zones, bool) {
	glog.V(1).Info("Claiming to support Zones")

	return os, true
}
func (os *OpenStack) GetZone() (cloudprovider.Zone, error) {
	glog.V(1).Infof("Current zone is %v", os.region)

	return cloudprovider.Zone{Region: os.region}, nil
}

func getServerByAddress(compute *gophercloud.ServiceClient, ip string) (*servers.Server, error) {
	opts := servers.ListOpts{
		Status: "ACTIVE",
	}
	pager := servers.List(compute, opts)

	serverList := make([]servers.Server, 0, 1)

	err := pager.EachPage(func(page pagination.Page) (bool, error) {
		s, err := servers.ExtractServers(page)
		if err != nil {
			return false, err
		}
		for _, server := range s {
			addr := findAddrs(server.Addresses, "fixed")[0]
			if addr == ip {
				serverList = append(serverList, server)
			}
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}

	if len(serverList) == 0 {
		return nil, ErrNotFound
	} else if len(serverList) > 1 {
		return nil, ErrMultipleResults
	}

	return &serverList[0], nil
}
