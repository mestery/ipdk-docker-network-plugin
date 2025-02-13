//
// Copyright (c) 2017 Intel Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package main

import (
	"bufio"
	"bytes"
	"encoding/gob"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/01org/ciao/uuid"
	"github.com/boltdb/bolt"
	"github.com/docker/libnetwork/drivers/remote/api"
	ipamapi "github.com/docker/libnetwork/ipams/remote/api"
	"github.com/golang/glog"
	"github.com/gorilla/mux"
	"github.com/joho/godotenv"
)

type epVal struct {
	IP            string
	vhostuserPort string //The dpdk vhost user port
	ipdkInterface  string
}

type nwVal struct {
	Bridge  string //The bridge on which the ports will be created
	Gateway net.IPNet
}

var intfCounter int

var epMap struct {
	sync.Mutex
	m map[string]*epVal
}

var nwMap struct {
	sync.Mutex
	m map[string]*nwVal
}

var brMap struct {
	sync.Mutex
	brCount int
	intfCount int
	m       map[string]int
}

var dbFile string
var db *bolt.DB

func init() {
	epMap.m = make(map[string]*epVal)
	nwMap.m = make(map[string]*nwVal)
	brMap.m = make(map[string]int)
	brMap.brCount = 1
	brMap.intfCount = 1
	dbFile = "/tmp/dpdk_bolt.db"
}

//We should never see any errors in this function
func sendResponse(resp interface{}, w http.ResponseWriter) {
	rb, err := json.Marshal(resp)
	if err != nil {
		glog.Errorf("unable to marshal response %v", err)
	}
	glog.Infof("Sending response := %v, %v", resp, err)
	fmt.Fprintf(w, "%s", rb)
	return
}

func getBody(r *http.Request) ([]byte, error) {
	body, err := ioutil.ReadAll(r.Body)
	glog.Infof("URL [%s] Body [%s] Error [%v]", r.URL.Path[1:], string(body), err)
	return body, err
}

func handler(w http.ResponseWriter, r *http.Request) {
	body, _ := getBody(r)
	resp := api.Response{}
	resp.Err = "Unhandled API request " + string(r.URL.Path[1:]) + " " + string(body)
	sendResponse(resp, w)
}

func handlerPluginActivate(w http.ResponseWriter, r *http.Request) {
	_, _ = getBody(r)
	//TODO: Where is this encoding?
	resp := `{
    "Implements": ["NetworkDriver", "IpamDriver"]
}`
	fmt.Fprintf(w, "%s", resp)
}

func handlerGetCapabilities(w http.ResponseWriter, r *http.Request) {
	_, _ = getBody(r)
	resp := api.GetCapabilityResponse{Scope: "local"}
	sendResponse(resp, w)
}

func handlerCreateNetwork(w http.ResponseWriter, r *http.Request) {
	resp := api.CreateNetworkResponse{}
	bridge := "br"

	body, err := getBody(r)
	if err != nil {
		resp.Err = "Error: " + err.Error()
		sendResponse(resp, w)
		return
	}

	req := api.CreateNetworkRequest{}
	if err := json.Unmarshal(body, &req); err != nil {
		resp.Err = "Error: " + err.Error()
		sendResponse(resp, w)
		return
	}

	nwMap.Lock()
	defer nwMap.Unlock()

	//Record the docker network UUID to SDN bridge mapping
	//This has to survive a plugin crash/restart and needs to be persisted
	nwMap.m[req.NetworkID] = &nwVal{
		Bridge:  bridge,
		Gateway: *req.IPv4Data[0].Gateway,
	}

	if err := dbAdd("nwMap", req.NetworkID, nwMap.m[req.NetworkID]); err != nil {
		glog.Errorf("Unable to update db %v", err)
	}

	// For IPDK, we are connecting endpoints via a bridge which requires
	// a unique integer ID.
	brMap.Lock()
	brMap.m[req.NetworkID] = brMap.brCount
	brMap.brCount = brMap.brCount + 1
	if err := dbAdd("brMap", req.NetworkID, brMap.m[req.NetworkID]); err != nil {
		glog.Errorf("Unable to update db %v", err)
	}
	brMap.Unlock()

	sendResponse(resp, w)
}

func handlerDeleteNetwork(w http.ResponseWriter, r *http.Request) {
	resp := api.DeleteNetworkResponse{}

	body, err := getBody(r)
	if err != nil {
		resp.Err = "Error: " + err.Error()
		sendResponse(resp, w)
		return
	}

	req := api.DeleteNetworkRequest{}
	if err = json.Unmarshal(body, &req); err != nil {
		resp.Err = "Error: " + err.Error()
		sendResponse(resp, w)
		return
	}

	glog.Infof("Delete Network := %v", req.NetworkID)

	nwMap.Lock()
	defer nwMap.Unlock()

	bridge := nwMap.m[req.NetworkID].Bridge
	delete(nwMap.m, req.NetworkID)
	if err := dbDelete("nwMap", req.NetworkID); err != nil {
		glog.Errorf("Unable to update db %v %v", err, bridge)
	}

	brMap.Lock()
	delete(brMap.m, req.NetworkID)
	if err := dbDelete("brMap", req.NetworkID); err != nil {
		glog.Errorf("Unable to update db %v %v", err, bridge)
	}
	brMap.Unlock()

	sendResponse(resp, w)
	return
}

func handlerEndpointOperInfof(w http.ResponseWriter, r *http.Request) {
	resp := api.EndpointInfoResponse{}
	body, err := getBody(r)

	if err != nil {
		resp.Err = "Error: " + err.Error()
		sendResponse(resp, w)
		return
	}

	req := api.EndpointInfoRequest{}
	err = json.Unmarshal(body, &req)

	if err != nil {
		resp.Err = "Error: " + err.Error()
		sendResponse(resp, w)
		return
	}

	sendResponse(resp, w)
}

func handlerCreateEndpoint(w http.ResponseWriter, r *http.Request) {
	resp := api.CreateEndpointResponse{}

	body, err := getBody(r)
	if err != nil {
		resp.Err = "Error: " + err.Error()
		sendResponse(resp, w)
		return
	}

	req := api.CreateEndpointRequest{}
	if err = json.Unmarshal(body, &req); err != nil {
		resp.Err = "Error: " + err.Error()
		sendResponse(resp, w)
		return
	}

	if req.Interface.Address == "" {
		resp.Err = "Error: IP Address parameter not provided in docker run"
		sendResponse(resp, w)
		return
	}

	ip, _, err := net.ParseCIDR(req.Interface.Address)
	if err != nil {
		resp.Err = "Error: Invalid IP Address " + err.Error()
		sendResponse(resp, w)
		return
	}

	nwMap.Lock()
	bridge := nwMap.m[req.NetworkID].Bridge
	nwMap.Unlock()

	if bridge == "" {
		resp.Err = "Error: incompatible network"
		sendResponse(resp, w)
		return
	}

	nwMap.Lock()
	defer nwMap.Unlock()

	epMap.Lock()
	defer epMap.Unlock()

	brMap.Lock()
	defer brMap.Unlock()

	//Generate a vhost-user port name to use with dummy interface.
	//We'll use the interfaces IP address
	vhostPort := fmt.Sprintf("%s", ip)

	//Create a unique path on the host to place the socket
	socketpath := fmt.Sprintf("/tmp/vhostuser_%s", ip)
	glog.Infof("INFO: Creating directory %v", socketpath)
	err = os.Mkdir(socketpath, 0755)
	if err != nil {
		resp.Err = fmt.Sprintf("Error making socket path %s: err: %v", socketpath, err)
		sendResponse(resp, w)
		return
	}

	// Create a unique name and host
	ipdk_intf := brMap.intfCount
	brMap.intfCount = brMap.intfCount + 1
	netnamet := fmt.Sprintf("net_vhost%d", ipdk_intf)
	netname := strings.Replace(netnamet, ".", "", -1)
	nethostt := fmt.Sprintf("host_%d", ipdk_intf)
	nethost := strings.Replace(nethostt, ".", "", -1)

	//Generate IPDK vhost-user interface:
	//docker exec -it ipdk gnmi-cli set "device:virtual-device,name:net_vhost0,host:host1,device-type:VIRTIO_NET,queues:1,socket-path:/tmp/vhost-user-0,port-type:LINK"
	cmd := "docker"
	args := []string{"exec", "ipdk", "gnmi-cli", "set", fmt.Sprintf("device:virtual-device,name:%s,host:%s,device-type:VIRTIO_NET,queues:1,socket-path:%s/vhu.sock,port-type:LINK", netname, nethost, socketpath)}
	glog.Infof("INFO: Running command [%v] with args [%v]", cmd, args)
	//if err := exec.Command(cmd, args...).Run(); err != nil {
	output, err := exec.Command(cmd, args...).Output()
	if err != nil {
		glog.Infof("ERROR: [%v] [%v] [%v] ", cmd, args, err)
		resp.Err = fmt.Sprintf("Error EndPointCreate: [%v] [%v] [%v]",
			cmd, args, err)
		sendResponse(resp, w)
		return
	}

	ifcb, _, err := bufio.NewReader(bytes.NewReader(output)).ReadLine()
	ifc := string(ifcb)

	glog.Infof("INFO: Result of gnmi-cli command [%v]", ifc)

	// Run ovs-p4ctl to add a pipeline entry
	cmd = "docker"
	args = []string{"exec", "ipdk", "ovs-p4ctl", "add-entry", "br0", "ingress.ipv4_host", fmt.Sprintf("hdr.ipv4.dst_addr=%s,action=ingress.send(%d)", ip, ipdk_intf)}
	glog.Infof("INFO: Running command [%v] with args [%v]", cmd, args)
	output, err = exec.Command(cmd, args...).Output()
	if err != nil {
		glog.Infof("ERROR: [%v] [%v] [%v] ", cmd, args, err)
		resp.Err = fmt.Sprintf("Error ovs-p4ctl : [%v] [%v] [%v]",
			cmd, args, err)
		sendResponse(resp, w)
		return
	}

	ifcb, _, err = bufio.NewReader(bytes.NewReader(output)).ReadLine()
	ifc = string(ifcb)

	glog.Infof("INFO: Result of ovs-p4ctl command [%v]", ifc)

	/* Setup the dummy interface corresponding to the dpdk port
	 * This is done so that docker CNM will program the IP Address
	 * and other properties on this Interface
	 * This dummy interface will be discovered by clear containers
	 * which then maps the actual vhost-user port to the VM
	 * This is needed today as docker does not pass any information
	 * from the network plugin to the runtime
	 */
	cmd = "ip"
	args = []string{"link", "add", vhostPort, "type", "dummy"}
	if err := exec.Command(cmd, args...).Run(); err != nil {
		resp.Err = fmt.Sprintf("Error EndPointCreate: [%v] [%v] [%v]",
			cmd, args, err)
		sendResponse(resp, w)
		return
	}

	glog.Infof("Setup dummy port %v %v ", cmd, args)

	epMap.m[req.EndpointID] = &epVal{
		IP:            req.Interface.Address,
		vhostuserPort: vhostPort,
		ipdkInterface:  fmt.Sprintf("%d", brMap.intfCount),
	}

	if err := dbAdd("epMap", req.EndpointID, epMap.m[req.EndpointID]); err != nil {
		glog.Errorf("Unable to update db %v %v", err, ip)
	}

	sendResponse(resp, w)
}

func handlerDeleteEndpoint(w http.ResponseWriter, r *http.Request) {
	resp := api.DeleteEndpointResponse{}

	body, err := getBody(r)
	if err != nil {
		resp.Err = "Error: " + err.Error()
		sendResponse(resp, w)
		return
	}

	req := api.DeleteEndpointRequest{}
	if err = json.Unmarshal(body, &req); err != nil {
		resp.Err = "Error: " + err.Error()
		sendResponse(resp, w)
		return
	}

	epMap.Lock()
	nwMap.Lock()

	m := epMap.m[req.EndpointID]
	vhostPort := m.vhostuserPort

	delete(epMap.m, req.EndpointID)
	if err := dbDelete("epMap", req.EndpointID); err != nil {
		glog.Errorf("Unable to update db %v %v", err, m)
	}
	nwMap.Unlock()
	epMap.Unlock()

	// Need to delete port using openconfig when we can

	//delete dummy port
	cmd := "ip"
	args := []string{"link", "del", vhostPort}
	glog.Infof("INFO: Deleting dummy port [%v]", vhostPort)
	if err := exec.Command(cmd, args...).Run(); err != nil {
		resp.Err = fmt.Sprintf("Error EndPointCreate: [%v] [%v] [%v]",
			cmd, args, err)
		sendResponse(resp, w)
		return
	}

	glog.Infof("Deleted dummy port %v %v ", cmd, args)

	// vhostPort contains the IP address
	glog.Infof("INFO: Removing directory and files at [/tmp/vhostuser_%v]", vhostPort)
	os.RemoveAll(fmt.Sprintf("/tmp/vhostuser_%s", vhostPort))
	if err != nil {
		glog.Infof("Couldn't remove /tmp/vhostuser_%s", vhostPort)
		resp.Err = fmt.Sprintf("Couldn't delete /tmp/vhostuser_%s: %v", vhostPort, err)
		sendResponse(resp, w)
		return
	}

	sendResponse(resp, w)
}

func handlerJoin(w http.ResponseWriter, r *http.Request) {
	resp := api.JoinResponse{}

	body, err := getBody(r)
	if err != nil {
		resp.Err = "Error: " + err.Error()
		sendResponse(resp, w)
		return
	}

	req := api.JoinRequest{}
	if err := json.Unmarshal(body, &req); err != nil {
		resp.Err = "Error: " + err.Error()
		sendResponse(resp, w)
		return
	}

	nwMap.Lock()
	epMap.Lock()
	nm := nwMap.m[req.NetworkID]
	em := epMap.m[req.EndpointID]
	nwMap.Unlock()
	epMap.Unlock()

	resp.Gateway = nm.Gateway.IP.String()
	resp.InterfaceName = &api.InterfaceName{
		SrcName:   em.vhostuserPort,
		DstPrefix: "eth",
	}
	glog.Infof("Join Response %v %v", resp, em.vhostuserPort)
	sendResponse(resp, w)
}

func handlerLeave(w http.ResponseWriter, r *http.Request) {
	resp := api.LeaveResponse{}

	body, err := getBody(r)
	if err != nil {
		resp.Err = "Error: " + err.Error()
		sendResponse(resp, w)
		return
	}

	req := api.LeaveRequest{}
	if err := json.Unmarshal(body, &req); err != nil {
		resp.Err = "Error: " + err.Error()
		sendResponse(resp, w)
		return
	}

	sendResponse(resp, w)
}

func handlerDiscoverNew(w http.ResponseWriter, r *http.Request) {
	resp := api.DiscoveryResponse{}

	body, err := getBody(r)
	if err != nil {
		resp.Err = "Error: " + err.Error()
		sendResponse(resp, w)
		return
	}

	req := api.DiscoveryNotification{}
	if err := json.Unmarshal(body, &req); err != nil {
		resp.Err = "Error: " + err.Error()
		sendResponse(resp, w)
		return
	}

	sendResponse(resp, w)
}

func handlerDiscoverDelete(w http.ResponseWriter, r *http.Request) {
	resp := api.DiscoveryResponse{}

	body, err := getBody(r)
	if err != nil {
		resp.Err = "Error: " + err.Error()
		sendResponse(resp, w)
		return
	}

	req := api.DiscoveryNotification{}
	if err := json.Unmarshal(body, &req); err != nil {
		resp.Err = "Error: " + err.Error()
		sendResponse(resp, w)
		return
	}

	sendResponse(resp, w)
}

func handlerExternalConnectivity(w http.ResponseWriter, r *http.Request) {
	resp := api.ProgramExternalConnectivityResponse{}

	body, err := getBody(r)
	if err != nil {
		resp.Err = "Error: " + err.Error()
		sendResponse(resp, w)
		return
	}

	req := api.ProgramExternalConnectivityRequest{}
	if err := json.Unmarshal(body, &req); err != nil {
		resp.Err = "Error: " + err.Error()
		sendResponse(resp, w)
		return
	}

	sendResponse(resp, w)
}

func handlerRevokeExternalConnectivity(w http.ResponseWriter, r *http.Request) {
	resp := api.RevokeExternalConnectivityResponse{}

	body, err := getBody(r)
	if err != nil {
		resp.Err = "Error: " + err.Error()
		sendResponse(resp, w)
		return
	}

	req := api.RevokeExternalConnectivityResponse{}
	if err := json.Unmarshal(body, &req); err != nil {
		resp.Err = "Error: " + err.Error()
		sendResponse(resp, w)
		return
	}

	sendResponse(resp, w)
}

func ipamGetCapabilities(w http.ResponseWriter, r *http.Request) {
	if _, err := getBody(r); err != nil {
		glog.Infof("ipamGetCapabilities: unable to get request body [%v]", err)
	}
	resp := ipamapi.GetCapabilityResponse{RequiresMACAddress: true}
	sendResponse(resp, w)
}

func ipamGetDefaultAddressSpaces(w http.ResponseWriter, r *http.Request) {
	resp := ipamapi.GetAddressSpacesResponse{}
	if _, err := getBody(r); err != nil {
		glog.Infof("ipamGetDefaultAddressSpaces: unable to get request body [%v]", err)
	}

	resp.GlobalDefaultAddressSpace = ""
	resp.LocalDefaultAddressSpace = ""
	sendResponse(resp, w)
}

func ipamRequestPool(w http.ResponseWriter, r *http.Request) {
	resp := ipamapi.RequestPoolResponse{}

	body, err := getBody(r)
	if err != nil {
		resp.Error = "Error: " + err.Error()
		sendResponse(resp, w)
		return
	}

	req := ipamapi.RequestPoolRequest{}
	if err := json.Unmarshal(body, &req); err != nil {
		resp.Error = "Error: " + err.Error()
		sendResponse(resp, w)
		return
	}

	resp.PoolID = uuid.Generate().String()
	resp.Pool = req.Pool
	sendResponse(resp, w)
}

func ipamReleasePool(w http.ResponseWriter, r *http.Request) {
	resp := ipamapi.ReleasePoolResponse{}

	body, err := getBody(r)
	if err != nil {
		resp.Error = "Error: " + err.Error()
		sendResponse(resp, w)
		return
	}

	req := ipamapi.ReleasePoolRequest{}
	if err := json.Unmarshal(body, &req); err != nil {
		resp.Error = "Error: " + err.Error()
		sendResponse(resp, w)
		return
	}

	sendResponse(resp, w)
}

func ipamRequestAddress(w http.ResponseWriter, r *http.Request) {
	resp := ipamapi.RequestAddressResponse{}

	body, err := getBody(r)
	if err != nil {
		resp.Error = "Error: " + err.Error()
		sendResponse(resp, w)
		return
	}

	req := ipamapi.RequestAddressRequest{}
	if err := json.Unmarshal(body, &req); err != nil {
		resp.Error = "Error: " + err.Error()
		sendResponse(resp, w)
		return
	}

	//TODO: Should come from the subnet mask for the subnet
	if req.Address != "" {
		resp.Address = req.Address + "/24"
	} else {
		resp.Error = "Error: Request does not have IP address. Specify using --ip"
	}
	sendResponse(resp, w)
}

func ipamReleaseAddress(w http.ResponseWriter, r *http.Request) {
	resp := ipamapi.ReleaseAddressResponse{}

	body, err := getBody(r)
	if err != nil {
		resp.Error = "Error: " + err.Error()
		sendResponse(resp, w)
		return
	}

	req := ipamapi.ReleaseAddressRequest{}
	if err := json.Unmarshal(body, &req); err != nil {
		resp.Error = "Error: " + err.Error()
		sendResponse(resp, w)
		return
	}

	sendResponse(resp, w)
}

func dbTableInit(tables []string) (err error) {

	glog.Infof("dbInit Tables := %v", tables)
	for i, v := range tables {
		glog.Infof("table[%v] := %v, %v", i, v, []byte(v))
	}

	err = db.Update(func(tx *bolt.Tx) error {
		for _, table := range tables {
			_, err := tx.CreateBucketIfNotExists([]byte(table))
			if err != nil {
				return fmt.Errorf("Bucket creation error: %v %v", table, err)
			}
		}
		return nil
	})

	if err != nil {
		glog.Errorf("Table creation error %v", err)
	}

	return err
}

func dbAdd(table string, key string, value interface{}) (err error) {

	err = db.Update(func(tx *bolt.Tx) error {
		var v bytes.Buffer

		if err := gob.NewEncoder(&v).Encode(value); err != nil {
			glog.Errorf("Encode Error: %v %v", err, value)
			return err
		}

		bucket := tx.Bucket([]byte(table))
		if bucket == nil {
			return fmt.Errorf("Bucket %v not found", table)
		}

		err = bucket.Put([]byte(key), v.Bytes())
		if err != nil {
			return fmt.Errorf("Key Store error: %v %v %v %v", table, key, value, err)
		}
		return nil
	})

	return err
}

func dbDelete(table string, key string) (err error) {

	err = db.Update(func(tx *bolt.Tx) error {

		bucket := tx.Bucket([]byte(table))
		if bucket == nil {
			return fmt.Errorf("Bucket %v not found", table)
		}

		err = bucket.Delete([]byte(key))
		if err != nil {
			return fmt.Errorf("Key Delete error: %v %v ", key, err)
		}
		return nil
	})

	return err
}

func dbGet(table string, key string) (value interface{}, err error) {

	err = db.View(func(tx *bolt.Tx) error {

		bucket := tx.Bucket([]byte(table))
		if bucket == nil {
			return fmt.Errorf("Bucket %v not found", table)
		}

		val := bucket.Get([]byte(key))
		if val == nil {
			return nil
		}

		v := bytes.NewReader(val)
		if err := gob.NewDecoder(v).Decode(value); err != nil {
			glog.Errorf("Decode Error: %v %v %v", table, key, err)
			return err
		}

		return nil
	})

	return value, err
}

func initDb() error {

	options := bolt.Options{
		Timeout: 3 * time.Second,
	}

	var err error
	db, err = bolt.Open(dbFile, 0644, &options)
	if err != nil {
		return fmt.Errorf("dbInit failed %v", err)
	}

	tables := []string{"global", "nwMap", "epMap", "brMap"}
	if err := dbTableInit(tables); err != nil {
		return fmt.Errorf("dbInit failed %v", err)
	}

	c, err := dbGet("global", "counter")
	if err != nil {
		glog.Errorf("dbGet failed %v", err)
		intfCounter = 100
	} else {
		var ok bool
		intfCounter, ok = c.(int)
		if !ok {
			intfCounter = 100
		}
	}

	err = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("nwMap"))

		err := b.ForEach(func(k, v []byte) error {
			vr := bytes.NewReader(v)
			nVal := &nwVal{}
			if err := gob.NewDecoder(vr).Decode(nVal); err != nil {
				return fmt.Errorf("Decode Error: %v %v %v", string(k), string(v), err)
			}
			nwMap.m[string(k)] = nVal
			glog.Infof("nwMap key=%v, value=%v\n", string(k), nVal)
			return nil
		})
		return err
	})

	if err != nil {
		return err
	}

	err = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("epMap"))

		err := b.ForEach(func(k, v []byte) error {
			vr := bytes.NewReader(v)
			eVal := &epVal{}
			if err := gob.NewDecoder(vr).Decode(eVal); err != nil {
				return fmt.Errorf("Decode Error: %v %v %v", string(k), string(v), err)
			}
			epMap.m[string(k)] = eVal
			glog.Infof("epMap key=%v, value=%v\n", string(k), eVal)
			return nil
		})
		return err
	})

	if err != nil {
		return err
	}

	err = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("brMap"))

		err := b.ForEach(func(k, v []byte) error {
			vr := bytes.NewReader(v)
			brVal := 0
			if err := gob.NewDecoder(vr).Decode(&brVal); err != nil {
				return fmt.Errorf("Decode Error: %v %v %v", string(k), string(v), err)
			}
			brMap.m[string(k)] = brVal
			glog.Infof("brMap key=%v, value=%v\n", string(k), brVal)
			return nil
		})
		return err
	})

	return err
}

func programP4() error {
	cmd := "docker"
	args := []string{"exec", "ipdk", "p4c", "--arch", "psa", "--target", "dpdk", "--output", "/root/examples/simple_l3/pipe", "--p4runtime-files", "/root/examples/simple_l3/p4Info.txt", "--bf-rt-schema", "/root/examples/simple_l3/bf-rt.json", "--context", "/root/examples/simple_l3/pipe/context.json", "/root/examples/simple_l3/simple_l3.p4"}
	glog.Infof("INFO: Running command [%v] with args [%v]", cmd, args)
	output, err := exec.Command(cmd, args...).Output()
	if err != nil {
		return fmt.Errorf("p4c building error [%v]", err)
	}

	ifcb, _, err := bufio.NewReader(bytes.NewReader(output)).ReadLine()
	ifc := string(ifcb)

	glog.Infof("INFO: Result of building p4c program [%v]", ifc)

	cmd = "docker"
	args = []string{"exec", "ipdk", "bash", "-c", "cd /root/examples/simple_l3 && ovs_pipeline_builder --p4c_conf_file=/root/examples/simple_l3/simple_l3.conf --bf_pipeline_config_binary_file=simple_l3.pb.bin"}
	glog.Infof("INFO: Running command [%v] with args [%v]", cmd, args)
	output, err = exec.Command(cmd, args...).Output()
	if err != nil {
		return fmt.Errorf("P4 programming error [%v]", err)
	}

	ifcb, _, err = bufio.NewReader(bytes.NewReader(output)).ReadLine()
	ifc = string(ifcb)

	glog.Infof("INFO: Result of P4 pipeline programming [%v]", ifc)

	cmd = "docker"
	args = []string{"exec", "ipdk", "bash", "-c", "cd /root/examples/simple_l3 && ovs-p4ctl set-pipe br0 /root/examples/simple_l3/simple_l3.pb.bin /root/examples/simple_l3/p4Info.txt"}
	glog.Infof("INFO: Running command [%v] with args [%v]", cmd, args)
	output, err = exec.Command(cmd, args...).Output()
	if err != nil {
		return fmt.Errorf("ovs-p4ctl error [%v]", err)
	}

	ifcb, _, err = bufio.NewReader(bytes.NewReader(output)).ReadLine()
	ifc = string(ifcb)

	glog.Infof("INFO: Result of ovs-p4ctl [%v]", ifc)

	return nil
}

func main() {
	flag.Parse()

	godotenv.Load("~/.ipdk/ipdk.env")

	if err := initDb(); err != nil {
		glog.Fatalf("db init failed, quitting [%v]", err)
	}
	defer func() {
		err := db.Close()
		glog.Errorf("unable to close database [%v]", err)
	}()

	r := mux.NewRouter()
	r.HandleFunc("/Plugin.Activate", handlerPluginActivate)
	r.HandleFunc("/NetworkDriver.GetCapabilities", handlerGetCapabilities)
	r.HandleFunc("/NetworkDriver.CreateNetwork", handlerCreateNetwork)
	r.HandleFunc("/NetworkDriver.DeleteNetwork", handlerDeleteNetwork)
	r.HandleFunc("/NetworkDriver.CreateEndpoint", handlerCreateEndpoint)
	r.HandleFunc("/NetworkDriver.DeleteEndpoint", handlerDeleteEndpoint)
	r.HandleFunc("/NetworkDriver.EndpointOperInfo", handlerEndpointOperInfof)
	r.HandleFunc("/NetworkDriver.Join", handlerJoin)
	r.HandleFunc("/NetworkDriver.Leave", handlerLeave)
	r.HandleFunc("/NetworkDriver.DiscoverNew", handlerDiscoverNew)
	r.HandleFunc("/NetworkDriver.DiscoverDelete", handlerDiscoverDelete)
	r.HandleFunc("/NetworkDriver.ProgramExternalConnectivity", handlerExternalConnectivity)
	r.HandleFunc("/NetworkDriver.RevokeExternalConnectivity", handlerRevokeExternalConnectivity)

	r.HandleFunc("/IpamDriver.GetCapabilities", ipamGetCapabilities)
	r.HandleFunc("/IpamDriver.GetDefaultAddressSpaces", ipamGetDefaultAddressSpaces)
	r.HandleFunc("/IpamDriver.RequestPool", ipamRequestPool)
	r.HandleFunc("/IpamDriver.ReleasePool", ipamReleasePool)
	r.HandleFunc("/IpamDriver.RequestAddress", ipamRequestAddress)

	r.HandleFunc("/", handler)
	err := http.ListenAndServe("127.0.0.1:9075", r)
	if err != nil {
		glog.Errorf("docker plugin http server failed, [%v]", err)
	}
}
