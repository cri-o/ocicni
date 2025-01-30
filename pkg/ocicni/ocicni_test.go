package ocicni

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/containernetworking/cni/libcni"
	"github.com/containernetworking/cni/pkg/types"
	cniv04 "github.com/containernetworking/cni/pkg/types/040"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/vishvananda/netlink"
)

func writeConfig(dir, fileName, netName, plugin, vers string) (conf, confPath string, err error) {
	confPath = filepath.Join(dir, fileName)
	conf = fmt.Sprintf(`{
	"name": "%s",
	"type": "%s",
	"cniVersion": "%s"
}`, netName, plugin, vers)

	return conf, confPath, os.WriteFile(confPath, []byte(conf), 0o644)
}

func writeCacheFile(dir, containerID, netName, ifname, config string) {
	cachedData := fmt.Sprintf(`{
	  "kind": "cniCacheV1",
	  "config": "%s",
	  "containerId": "%s",
	  "ifName": "%s",
	  "networkName": "%s",
	  "result": {
	    "cniVersion": "0.4.0"
	  }
	}`, base64.StdEncoding.EncodeToString([]byte(config)), containerID, ifname, netName)

	dirName := filepath.Join(dir, "results")
	err := os.MkdirAll(dirName, 0o700)
	Expect(err).NotTo(HaveOccurred())

	filePath := filepath.Join(dirName, fmt.Sprintf("%s-%s-%s", netName, containerID, ifname))
	err = os.WriteFile(filePath, []byte(cachedData), 0o644)
	Expect(err).NotTo(HaveOccurred())
}

type fakePlugin struct {
	expectedEnv  []string
	expectedConf string
	result       types.Result
	err          error
}

type fakeExec struct {
	version.PluginDecoder

	addIndex int
	delIndex int
	chkIndex int
	gcIndex  int
	plugins  []*fakePlugin

	failFind bool

	failStatus bool
}

type TestConf struct {
	CNIVersion string `json:"cniVersion,omitempty"`
	Name       string `json:"name,omitempty"`
	Type       string `json:"type,omitempty"`

	ValidAttachments []types.GCAttachment `json:"cni.dev/valid-attachments,omitempty"`
}

func (f *fakeExec) addPlugin(expectedEnv []string, expectedConf string, result types.Result) {
	f.plugins = append(f.plugins, &fakePlugin{
		expectedEnv:  expectedEnv,
		expectedConf: expectedConf,
		result:       result,
	})
}

// Ensure everything in needles is also present in haystack.
func matchArray(needles, haystack []string) {
	Expect(len(needles)).To(BeNumerically("<=", len(haystack)))

	for _, e1 := range needles {
		found := ""

		for _, e2 := range haystack {
			if e1 == e2 {
				found = e2

				break
			}
		}
		// Compare element values for more descriptive test failure
		Expect(e1).To(Equal(found))
	}
}

func getCNICommand(env []string) (string, error) {
	for _, e := range env {
		parts := strings.Split(e, "=")
		if len(parts) == 2 && parts[0] == "CNI_COMMAND" {
			return parts[1], nil
		}
	}

	return "", errors.New("failed to find CNI_COMMAND")
}

func (f *fakeExec) ExecPlugin(ctx context.Context, pluginPath string, stdinData []byte, environ []string) ([]byte, error) {
	cmd, err := getCNICommand(environ)
	Expect(err).NotTo(HaveOccurred())

	var index int

	switch cmd {
	case "ADD":
		Expect(len(f.plugins)).To(BeNumerically(">", f.addIndex))
		index = f.addIndex
		f.addIndex++
	case "DEL":
		Expect(len(f.plugins)).To(BeNumerically(">", f.delIndex))
		index = f.delIndex
		f.delIndex++
	case "CHECK":
		Expect(f.plugins).To(HaveLen(f.addIndex))
		index = f.chkIndex
		f.chkIndex++
	case "GC":
		Expect(len(f.plugins)).To(BeNumerically(">", f.gcIndex))
		index = f.gcIndex
		f.gcIndex++
	case "VERSION":
		// Just return all supported versions
		return json.Marshal(version.All)
	case "STATUS":
		if f.failStatus {
			return nil, errors.New("status fails")
		}

		return nil, nil
	default:
		// Should never be reached
		Expect(false).To(BeTrue())
	}

	plugin := f.plugins[index]

	GinkgoT().Logf("[%s %d] exec plugin %q found %+v", cmd, index, pluginPath, plugin)

	// SetUpPod We only care about a few fields
	testConf := &TestConf{}
	err = json.Unmarshal(stdinData, &testConf)
	Expect(err).NotTo(HaveOccurred())

	testData, err := json.Marshal(testConf)
	Expect(err).NotTo(HaveOccurred())

	if plugin.expectedConf != "" {
		Expect(string(testData)).To(MatchJSON(plugin.expectedConf))
	}

	if len(plugin.expectedEnv) > 0 {
		matchArray(plugin.expectedEnv, environ)
	}

	if plugin.err != nil {
		return nil, plugin.err
	}

	resultJSON := []byte{}
	if plugin.result != nil {
		resultJSON, err = json.Marshal(plugin.result)
		Expect(err).NotTo(HaveOccurred())
	}

	return resultJSON, nil
}

func (f *fakeExec) FindInPath(plugin string, paths []string) (string, error) {
	Expect(paths).ToNot(BeEmpty())

	if f.failFind {
		return "", fmt.Errorf("failed to find plugin %q in path %s", plugin, paths)
	}

	return filepath.Join(paths[0], plugin), nil
}

func ensureCIDR(cidr string) *net.IPNet {
	ip, network, err := net.ParseCIDR(cidr)
	Expect(err).NotTo(HaveOccurred())

	network.IP = ip

	return network
}

var _ = Describe("ocicni operations", func() {
	var (
		tmpDir    string
		tmpBinDir string
		cacheDir  string
		networkNS ns.NetNS
	)

	BeforeEach(func() {
		var err error
		tmpDir, err = os.MkdirTemp("", "ocicni_tmp")
		Expect(err).NotTo(HaveOccurred())
		tmpBinDir, err = os.MkdirTemp("", "ocicni_tmp_bin")
		Expect(err).NotTo(HaveOccurred())
		cacheDir, err = os.MkdirTemp("", "ocicni_cache")
		Expect(err).NotTo(HaveOccurred())
		err = os.Mkdir(filepath.Join(cacheDir, "results"), 0o700)
		Expect(err).NotTo(HaveOccurred())

		networkNS, err = testutils.NewNS()
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		Expect(networkNS.Close()).To(Succeed())
		Expect(testutils.UnmountNS(networkNS)).To(Succeed())

		err := os.RemoveAll(tmpDir)
		Expect(err).NotTo(HaveOccurred())
		err = os.RemoveAll(tmpBinDir)
		Expect(err).NotTo(HaveOccurred())
		err = os.RemoveAll(cacheDir)
		Expect(err).NotTo(HaveOccurred())
	})

	It("finds an existing default network configuration", func() {
		_, _, err := writeConfig(tmpDir, "5-notdefault.conf", "notdefault", "myplugin", "0.3.1")
		Expect(err).NotTo(HaveOccurred())
		_, _, err = writeConfig(tmpDir, "10-test.conf", "test", "myplugin", "0.3.1")
		Expect(err).NotTo(HaveOccurred())

		ocicni, err := initCNI(&fakeExec{}, "", "test", tmpDir, false, "/opt/cni/bin")
		Expect(err).NotTo(HaveOccurred())
		Expect(ocicni.Status()).NotTo(HaveOccurred())

		// Ensure the default network is the one we expect
		tmp, ok := ocicni.(*cniNetworkPlugin)
		Expect(ok).To(BeTrue())
		net := tmp.getDefaultNetwork()
		Expect(net.name).To(Equal("test"))
		Expect(net.config.Plugins).ToNot(BeEmpty())
		Expect(net.config.Plugins[0].Network.Type).To(Equal("myplugin"))

		Expect(ocicni.Shutdown()).NotTo(HaveOccurred())
	})

	It("finds an asynchronously written default network configuration", func() {
		ocicni, err := initCNI(&fakeExec{}, "", "test", tmpDir, true, "/opt/cni/bin")
		Expect(err).NotTo(HaveOccurred())

		// Writing a config that doesn't match the default network
		_, _, err = writeConfig(tmpDir, "5-notdefault.conf", "notdefault", "myplugin", "0.3.1")
		Expect(err).NotTo(HaveOccurred())
		Consistently(ocicni.Status, 5).Should(HaveOccurred())

		_, _, err = writeConfig(tmpDir, "10-test.conf", "test", "myplugin", "0.3.1")
		Expect(err).NotTo(HaveOccurred())
		Eventually(ocicni.Status, 5).Should(Succeed())

		tmp, ok := ocicni.(*cniNetworkPlugin)
		Expect(ok).To(BeTrue())
		net := tmp.getDefaultNetwork()
		Expect(net.name).To(Equal("test"))
		Expect(net.config.Plugins).ToNot(BeEmpty())
		Expect(net.config.Plugins[0].Network.Type).To(Equal("myplugin"))

		Expect(ocicni.Shutdown()).NotTo(HaveOccurred())
	})

	It("finds an asynchronously written default network configuration whose plugin is written later", func() {
		fExec := &fakeExec{failFind: true}
		ocicni, err := initCNI(fExec, "", "test", tmpDir, true, tmpBinDir)
		Expect(err).NotTo(HaveOccurred())

		_, _, err = writeConfig(tmpDir, "10-test.conf", "test", "myplugin", "0.3.1")
		Expect(err).NotTo(HaveOccurred())
		Consistently(ocicni.Status, 5).ShouldNot(Succeed())

		// Write a file in the bindir to trigger the fsnotify code to resync
		fExec.failFind = false
		err = os.WriteFile(filepath.Join(tmpBinDir, "myplugin"), []byte("adsfasdfsafd"), 0o755)
		Expect(err).NotTo(HaveOccurred())

		tmp, ok := ocicni.(*cniNetworkPlugin)
		Expect(ok).To(BeTrue())
		Eventually(func() error {
			net := tmp.getDefaultNetwork()
			if net == nil {
				return errors.New("no default net")
			}
			if net.name != "test" {
				return errors.New("name not test")
			}
			if len(net.config.Plugins) == 0 {
				return errors.New("no plugins")
			}
			if net.config.Plugins[0].Network.Type != "myplugin" {
				return errors.New("wrong plugin type")
			}

			return nil
		}, 10).Should(Succeed())

		Expect(ocicni.Shutdown()).NotTo(HaveOccurred())
	})

	//nolint:dupl // no need to dedup for tests
	It("should monitor the net conf dir for changes when default network is not specified", func() {
		_, _, err := writeConfig(tmpDir, "5-notdefault.conf", "notdefault", "myplugin", "0.3.1")
		Expect(err).NotTo(HaveOccurred())
		_, _, err = writeConfig(tmpDir, "10-test.conf", "test", "myplugin", "0.3.1")
		Expect(err).NotTo(HaveOccurred())

		ocicni, err := initCNI(&fakeExec{}, "", "", tmpDir, true, "/opt/cni/bin")
		Expect(err).NotTo(HaveOccurred())
		Expect(ocicni.Status()).NotTo(HaveOccurred())

		// Ensure the default network is the one we expect
		tmp, ok := ocicni.(*cniNetworkPlugin)
		Expect(ok).To(BeTrue())
		net := tmp.getDefaultNetwork()
		Expect(net.name).To(Equal("test"))
		Expect(net.config.Plugins).ToNot(BeEmpty())
		Expect(net.config.Plugins[0].Network.Type).To(Equal("myplugin"))

		// If a CNI config file is updated, default network name should be reloaded real-time
		_, _, err = writeConfig(tmpDir, "10-test.conf", "secondary", "testplugin", "0.3.1")
		Expect(err).NotTo(HaveOccurred())

		Consistently(ocicni.Status, 5).Should(Succeed())

		net = tmp.getDefaultNetwork()
		Expect(net.name).To(Equal("secondary"))
		Expect(net.config.Plugins).ToNot(BeEmpty())
		Expect(net.config.Plugins[0].Network.Type).To(Equal("testplugin"))

		Expect(ocicni.Shutdown()).NotTo(HaveOccurred())
	})

	//nolint:dupl // no need to dedup for tests
	It("should monitor the net conf dir for changes when default network is specified", func() {
		_, _, err := writeConfig(tmpDir, "5-notdefault.conf", "notdefault", "myplugin", "0.3.1")
		Expect(err).NotTo(HaveOccurred())
		_, _, err = writeConfig(tmpDir, "10-test.conf", "test", "myplugin", "0.3.1")
		Expect(err).NotTo(HaveOccurred())

		ocicni, err := initCNI(&fakeExec{}, "", "test", tmpDir, true, "/opt/cni/bin")
		Expect(err).NotTo(HaveOccurred())
		Expect(ocicni.Status()).NotTo(HaveOccurred())

		// Ensure the default network is the one we expect
		tmp, ok := ocicni.(*cniNetworkPlugin)
		Expect(ok).To(BeTrue())
		net := tmp.getDefaultNetwork()
		Expect(net.name).To(Equal("test"))
		Expect(net.config.Plugins).ToNot(BeEmpty())
		Expect(net.config.Plugins[0].Network.Type).To(Equal("myplugin"))

		// If a CNI config file is updated, default network name should be reloaded real-time
		_, _, err = writeConfig(tmpDir, "10-test.conf", "test", "testplugin", "0.3.1")
		Expect(err).NotTo(HaveOccurred())

		Consistently(ocicni.Status, 5).Should(Succeed())

		net = tmp.getDefaultNetwork()
		Expect(net.name).To(Equal("test"))
		Expect(net.config.Plugins).ToNot(BeEmpty())
		Expect(net.config.Plugins[0].Network.Type).To(Equal("testplugin"))

		Expect(ocicni.Shutdown()).NotTo(HaveOccurred())
	})

	It("finds and refinds an asynchronously written default network configuration", func() {
		f := &fakeExec{}
		ocicni, err := initCNI(f, "", "test", tmpDir, true, "/opt/cni/bin")
		Expect(err).NotTo(HaveOccurred())

		err = ocicni.Status()
		Expect(err).To(HaveOccurred())

		// Write the default network config
		_, confPath, err := writeConfig(tmpDir, "10-test.conf", "test", "myplugin", "0.3.1")
		Expect(err).NotTo(HaveOccurred())
		Eventually(ocicni.Status, 5).Should(Succeed())

		tmp, ok := ocicni.(*cniNetworkPlugin)
		Expect(ok).To(BeTrue())
		net := tmp.getDefaultNetwork()
		Expect(net.name).To(Equal("test"))
		Expect(net.config.Plugins).ToNot(BeEmpty())
		Expect(net.config.Plugins[0].Network.Type).To(Equal("myplugin"))

		// Delete the default network config, ensure ocicni begins to
		// return a status error
		err = os.Remove(confPath)
		Expect(err).NotTo(HaveOccurred())
		Eventually(ocicni.Status, 5).Should(HaveOccurred())

		// Write the default network config again, this time with v1.1, and wait for status
		// to be OK
		_, _, err = writeConfig(tmpDir, "10-test.conf", "test", "myplugin", "1.1.0")
		Expect(err).NotTo(HaveOccurred())
		Eventually(ocicni.Status, 5).Should(Succeed())

		f.failStatus = true
		err = ocicni.Status()
		Expect(err).To(HaveOccurred())

		f.failStatus = false
		err = ocicni.Status()
		Expect(err).NotTo(HaveOccurred())

		Expect(ocicni.Shutdown()).NotTo(HaveOccurred())
	})

	It("finds an ASCIIbetically first network configuration as default real-time if given no default network name", func() {
		ocicni, err := initCNI(&fakeExec{}, "", "", tmpDir, true, "/opt/cni/bin")
		Expect(err).NotTo(HaveOccurred())

		_, _, err = writeConfig(tmpDir, "15-test.conf", "test", "myplugin", "0.3.1")
		Expect(err).NotTo(HaveOccurred())
		_, _, err = writeConfig(tmpDir, "5-notdefault.conf", "notdefault", "myplugin", "0.3.1")
		Expect(err).NotTo(HaveOccurred())

		Eventually(ocicni.Status, 5).Should(Succeed())

		tmp, ok := ocicni.(*cniNetworkPlugin)
		Expect(ok).To(BeTrue())
		net := tmp.getDefaultNetwork()
		Expect(net.name).To(Equal("test"))
		Expect(net.config.Plugins).ToNot(BeEmpty())
		Expect(net.config.Plugins[0].Network.Type).To(Equal("myplugin"))

		// If a new CNI config file is added, default network name should be reloaded real-time
		// by file sorting
		_, _, err = writeConfig(tmpDir, "10-abc.conf", "newdefault", "myplugin", "0.3.1")
		Expect(err).NotTo(HaveOccurred())

		Consistently(ocicni.Status, 5).Should(Succeed())

		net = tmp.getDefaultNetwork()
		Expect(net.name).To(Equal("newdefault"))
		Expect(net.config.Plugins).ToNot(BeEmpty())
		Expect(net.config.Plugins[0].Network.Type).To(Equal("myplugin"))

		Expect(ocicni.Shutdown()).NotTo(HaveOccurred())
	})

	It("returns correct default network from loadNetworks()", func() {
		// Writing a config that doesn't match the default network
		_, _, err := writeConfig(tmpDir, "5-network1.conf", "network1", "myplugin", "0.3.1")
		Expect(err).NotTo(HaveOccurred())
		_, _, err = writeConfig(tmpDir, "10-network2.conf", "network2", "myplugin", "0.3.1")
		Expect(err).NotTo(HaveOccurred())
		_, _, err = writeConfig(tmpDir, "30-network3.conf", "network3", "myplugin", "0.3.1")
		Expect(err).NotTo(HaveOccurred())
		_, _, err = writeConfig(tmpDir, "afdsfdsafdsa-network3.conf", "network4", "myplugin", "0.3.1")
		Expect(err).NotTo(HaveOccurred())

		cniConfig := libcni.NewCNIConfig([]string{"/opt/cni/bin"}, &fakeExec{})
		netMap, defname, err := loadNetworks(context.TODO(), tmpDir, cniConfig)
		Expect(err).NotTo(HaveOccurred())
		Expect(netMap).To(HaveLen(4))
		// filenames are sorted asciibetically
		Expect(defname).To(Equal("network2"))
	})

	It("returns no error from loadNetworks() when no config files exist", func() {
		cniConfig := libcni.NewCNIConfig([]string{"/opt/cni/bin"}, &fakeExec{})
		netMap, defname, err := loadNetworks(context.TODO(), tmpDir, cniConfig)
		Expect(err).NotTo(HaveOccurred())
		Expect(netMap).To(BeEmpty())
		// filenames are sorted asciibetically
		Expect(defname).To(Equal(""))
	})

	It("ignores subsequent duplicate network names in loadNetworks()", func() {
		// Writing a config that doesn't match the default network
		_, _, err := writeConfig(tmpDir, "10-network2.conf", "network2", "myplugin", "0.3.1")
		Expect(err).NotTo(HaveOccurred())
		_, _, err = writeConfig(tmpDir, "30-network3.conf", "network3", "myplugin", "0.3.1")
		Expect(err).NotTo(HaveOccurred())
		_, _, err = writeConfig(tmpDir, "5-network1.conf", "network2", "myplugin2", "0.3.1")
		Expect(err).NotTo(HaveOccurred())

		cniConfig := libcni.NewCNIConfig([]string{"/opt/cni/bin"}, &fakeExec{})
		netMap, _, err := loadNetworks(context.TODO(), tmpDir, cniConfig)
		Expect(err).NotTo(HaveOccurred())

		// We expect the type=myplugin2 network be ignored since it
		// was read earlier than the type=myplugin network with the same name
		Expect(netMap).To(HaveLen(2))
		net, ok := netMap["network2"]
		Expect(ok).To(BeTrue())
		Expect(net.config.Plugins[0].Network.Type).To(Equal("myplugin"))
	})

	It("build different runtime configs", func() {
		ifName := "eth0"
		podNetwork := &PodNetwork{}

		var (
			runtimeConfig *RuntimeConfig
			rt            *libcni.RuntimeConf
			err           error
		)

		// empty runtimeConfig
		_, err = buildCNIRuntimeConf(podNetwork, ifName, runtimeConfig)
		Expect(err).NotTo(HaveOccurred())

		// runtimeConfig with invalid IP
		runtimeConfig = &RuntimeConfig{IP: "172.16"}
		_, err = buildCNIRuntimeConf(podNetwork, ifName, runtimeConfig)
		Expect(err).To(HaveOccurred())

		// runtimeConfig with valid IP
		runtimeConfig = &RuntimeConfig{IP: "172.16.0.1"}
		rt, err = buildCNIRuntimeConf(podNetwork, ifName, runtimeConfig)
		Expect(err).NotTo(HaveOccurred())
		Expect(rt.Args).To(HaveLen(6))
		Expect(rt.Args[5][1]).To(Equal("172.16.0.1"))

		// runtimeConfig with invalid MAC
		runtimeConfig = &RuntimeConfig{MAC: "f0:a6"}
		_, err = buildCNIRuntimeConf(podNetwork, ifName, runtimeConfig)
		Expect(err).To(HaveOccurred())

		// runtimeConfig with valid MAC
		runtimeConfig = &RuntimeConfig{MAC: "9e:0c:d9:b2:f0:a6"}
		rt, err = buildCNIRuntimeConf(podNetwork, ifName, runtimeConfig)
		Expect(err).NotTo(HaveOccurred())
		Expect(rt.Args).To(HaveLen(6))
		Expect(rt.Args[5][1]).To(Equal("9e:0c:d9:b2:f0:a6"))

		// runtimeConfig with valid IP and valid MAC
		runtimeConfig = &RuntimeConfig{IP: "172.16.0.1", MAC: "9e:0c:d9:b2:f0:a6"}
		rt, err = buildCNIRuntimeConf(podNetwork, ifName, runtimeConfig)
		Expect(err).NotTo(HaveOccurred())
		Expect(rt.Args).To(HaveLen(7))
		Expect(rt.Args[5][1]).To(Equal("172.16.0.1"))
		Expect(rt.Args[6][1]).To(Equal("9e:0c:d9:b2:f0:a6"))

		// runtimeConfig with portMappings is nil
		runtimeConfig = &RuntimeConfig{PortMappings: nil}
		_, err = buildCNIRuntimeConf(podNetwork, ifName, runtimeConfig)
		Expect(err).NotTo(HaveOccurred())

		// runtimeConfig with valid portMappings
		runtimeConfig = &RuntimeConfig{PortMappings: []PortMapping{{
			HostPort:      100,
			ContainerPort: 50,
			Protocol:      "tcp",
			HostIP:        "192.168.0.1",
		}}}
		rt, err = buildCNIRuntimeConf(podNetwork, ifName, runtimeConfig)
		Expect(err).NotTo(HaveOccurred())
		pm, ok := rt.CapabilityArgs["portMappings"].([]PortMapping)
		Expect(ok).To(BeTrue())
		Expect(pm).To(HaveLen(1))
		Expect(pm[0].HostPort).To(Equal(int32(100)))
		Expect(pm[0].ContainerPort).To(Equal(int32(50)))
		Expect(pm[0].Protocol).To(Equal("tcp"))
		Expect(pm[0].HostIP).To(Equal("192.168.0.1"))

		// runtimeConfig with bandwidth is nil
		runtimeConfig = &RuntimeConfig{Bandwidth: nil}
		_, err = buildCNIRuntimeConf(podNetwork, ifName, runtimeConfig)
		Expect(err).NotTo(HaveOccurred())

		// runtimeConfig with valid bandwidth
		runtimeConfig = &RuntimeConfig{Bandwidth: &BandwidthConfig{
			IngressRate:  1,
			IngressBurst: 2,
			EgressRate:   3,
			EgressBurst:  4,
		}}
		rt, err = buildCNIRuntimeConf(podNetwork, ifName, runtimeConfig)
		Expect(err).NotTo(HaveOccurred())
		bw, ok := rt.CapabilityArgs["bandwidth"].(map[string]uint64)
		Expect(ok).To(BeTrue())
		Expect(bw["ingressRate"]).To(Equal(uint64(1)))
		Expect(bw["ingressBurst"]).To(Equal(uint64(2)))
		Expect(bw["egressRate"]).To(Equal(uint64(3)))
		Expect(bw["egressBurst"]).To(Equal(uint64(4)))

		// runtimeConfig with ipRanges is empty
		runtimeConfig = &RuntimeConfig{IpRanges: [][]IpRange{}}
		_, err = buildCNIRuntimeConf(podNetwork, ifName, runtimeConfig)
		Expect(err).NotTo(HaveOccurred())

		// runtimeConfig with valid ipRanges
		runtimeConfig = &RuntimeConfig{IpRanges: [][]IpRange{{IpRange{
			Subnet:     "192.168.0.0/24",
			RangeStart: "192.168.0.100",
			RangeEnd:   "192.168.0.200",
			Gateway:    "192.168.0.254",
		}}}}
		rt, err = buildCNIRuntimeConf(podNetwork, ifName, runtimeConfig)
		Expect(err).NotTo(HaveOccurred())
		ir, ok := rt.CapabilityArgs["ipRanges"].([][]IpRange)
		Expect(ok).To(BeTrue())
		Expect(ir).To(HaveLen(1))
		Expect(ir[0]).To(HaveLen(1))
		Expect(ir[0][0].Gateway).To(Equal("192.168.0.254"))

		runtimeConfig = &RuntimeConfig{CgroupPath: "/slice/pod/testing"}
		rt, err = buildCNIRuntimeConf(podNetwork, ifName, runtimeConfig)
		Expect(err).NotTo(HaveOccurred())
		cg, ok := rt.CapabilityArgs["cgroupPath"].(string)
		Expect(ok).To(BeTrue())
		Expect(cg).To(Equal("/slice/pod/testing"))
	})

	It("sets up and tears down a pod using the default network", func() {
		conf, _, err := writeConfig(tmpDir, "10-network2.conf", "network2", "myplugin", "0.3.1")
		Expect(err).NotTo(HaveOccurred())

		fake := &fakeExec{}
		expectedResult := &cniv04.Result{
			CNIVersion: "0.3.1",
			Interfaces: []*cniv04.Interface{
				{
					Name:    "eth0",
					Mac:     "01:23:45:67:89:01",
					Sandbox: networkNS.Path(),
				},
			},
			IPs: []*cniv04.IPConfig{
				{
					Interface: cniv04.Int(0),
					Version:   "4",
					Address:   *ensureCIDR("1.1.1.2/24"),
				},
			},
		}
		fake.addPlugin(nil, conf, expectedResult)

		ocicni, err := initCNI(fake, cacheDir, "network2", tmpDir, true, "/opt/cni/bin")
		Expect(err).NotTo(HaveOccurred())

		podNet := PodNetwork{
			Name:      "pod1",
			Namespace: "namespace1",
			ID:        "1234567890",
			UID:       "9414bd03-b3d3-453e-9d9f-47dcee07958c",
			NetNS:     networkNS.Path(),
		}
		results, err := ocicni.SetUpPod(podNet)
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.addIndex).To(Equal(len(fake.plugins)))
		Expect(results).To(HaveLen(1))
		r, ok := results[0].Result.(*cniv04.Result)
		Expect(ok).To(BeTrue())
		Expect(reflect.DeepEqual(r, expectedResult)).To(BeTrue())

		// Make sure loopback device is up
		err = networkNS.Do(func(_ ns.NetNS) error {
			defer GinkgoRecover()
			link, err := netlink.LinkByName("lo")
			Expect(err).NotTo(HaveOccurred())
			Expect(link.Attrs().Flags & net.FlagUp).To(Equal(net.FlagUp))

			return nil
		})
		Expect(err).NotTo(HaveOccurred())

		err = ocicni.TearDownPod(podNet)
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.delIndex).To(Equal(len(fake.plugins)))

		Expect(ocicni.Shutdown()).NotTo(HaveOccurred())
	})

	It("sets up and tears down a pod using specified networks", func() {
		_, _, err := writeConfig(tmpDir, "10-network2.conf", "network2", "myplugin", "0.3.1")
		Expect(err).NotTo(HaveOccurred())

		conf1, _, err := writeConfig(tmpDir, "20-network3.conf", "network3", "myplugin", "0.3.1")
		Expect(err).NotTo(HaveOccurred())
		conf2, _, err := writeConfig(tmpDir, "30-network4.conf", "network4", "myplugin", "0.3.1")
		Expect(err).NotTo(HaveOccurred())

		fake := &fakeExec{}
		expectedResult1 := &cniv04.Result{
			CNIVersion: "0.3.1",
			Interfaces: []*cniv04.Interface{
				{
					Name:    "eth0",
					Mac:     "01:23:45:67:89:01",
					Sandbox: networkNS.Path(),
				},
			},
			IPs: []*cniv04.IPConfig{
				{
					Interface: cniv04.Int(0),
					Version:   "4",
					Address:   *ensureCIDR("1.1.1.2/24"),
				},
			},
		}
		fake.addPlugin(nil, conf1, expectedResult1)

		expectedResult2 := &cniv04.Result{
			CNIVersion: "0.3.1",
			Interfaces: []*cniv04.Interface{
				{
					Name:    "eth1",
					Mac:     "01:23:45:67:89:02",
					Sandbox: networkNS.Path(),
				},
			},
			IPs: []*cniv04.IPConfig{
				{
					Interface: cniv04.Int(0),
					Address:   *ensureCIDR("1.1.1.3/24"),
				},
			},
		}
		fake.addPlugin(nil, conf2, expectedResult2)

		ocicni, err := initCNI(fake, cacheDir, "network2", tmpDir, true, "/opt/cni/bin")
		Expect(err).NotTo(HaveOccurred())

		podNet := PodNetwork{
			Name:      "pod1",
			Namespace: "namespace1",
			ID:        "1234567890",
			UID:       "9414bd03-b3d3-453e-9d9f-47dcee07958c",
			NetNS:     networkNS.Path(),
			Networks: []NetAttachment{
				{Name: "network3"},
				{Name: "network4"},
			},
		}
		results, err := ocicni.SetUpPod(podNet)
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.addIndex).To(Equal(len(fake.plugins)))
		Expect(results).To(HaveLen(2))
		r, ok := results[0].Result.(*cniv04.Result)
		Expect(ok).To(BeTrue())
		Expect(reflect.DeepEqual(r, expectedResult1)).To(BeTrue())
		r, ok = results[1].Result.(*cniv04.Result)
		Expect(ok).To(BeTrue())
		Expect(reflect.DeepEqual(r, expectedResult2)).To(BeTrue())

		err = ocicni.TearDownPod(podNet)
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.delIndex).To(Equal(len(fake.plugins)))

		Expect(ocicni.Shutdown()).NotTo(HaveOccurred())
	})

	It("sets up and tears down a pod using specified v4 networks", func() {
		_, _, err := writeConfig(tmpDir, "10-network2.conf", "network2", "myplugin", "0.4.0")
		Expect(err).NotTo(HaveOccurred())

		conf1, _, err := writeConfig(tmpDir, "20-network3.conf", "network3", "myplugin", "0.4.0")
		Expect(err).NotTo(HaveOccurred())
		conf2, _, err := writeConfig(tmpDir, "30-network4.conf", "network4", "myplugin", "0.4.0")
		Expect(err).NotTo(HaveOccurred())

		fake := &fakeExec{}
		expectedResult1 := &cniv04.Result{
			CNIVersion: "0.4.0",
			Interfaces: []*cniv04.Interface{
				{
					Name:    "eth0",
					Mac:     "01:23:45:67:89:01",
					Sandbox: networkNS.Path(),
				},
			},
			IPs: []*cniv04.IPConfig{
				{
					Interface: cniv04.Int(0),
					Version:   "4",
					Address:   *ensureCIDR("1.1.1.2/24"),
				},
			},
		}
		fake.addPlugin(nil, conf1, expectedResult1)

		expectedResult2 := &cniv04.Result{
			CNIVersion: "0.4.0",
			Interfaces: []*cniv04.Interface{
				{
					Name:    "eth1",
					Mac:     "01:23:45:67:89:02",
					Sandbox: networkNS.Path(),
				},
			},
			IPs: []*cniv04.IPConfig{
				{
					Interface: cniv04.Int(0),
					Version:   "4",
					Address:   *ensureCIDR("1.1.1.3/24"),
				},
			},
		}
		fake.addPlugin(nil, conf2, expectedResult2)

		ocicni, err := initCNI(fake, cacheDir, "network2", tmpDir, true, "/opt/cni/bin")
		Expect(err).NotTo(HaveOccurred())

		podNet := PodNetwork{
			Name:      "pod1",
			Namespace: "namespace1",
			ID:        "1234567890",
			UID:       "9414bd03-b3d3-453e-9d9f-47dcee07958c",
			NetNS:     networkNS.Path(),
			Networks: []NetAttachment{
				{Name: "network3"},
				{Name: "network4"},
			},
		}
		results, err := ocicni.SetUpPod(podNet)
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.addIndex).To(Equal(len(fake.plugins)))
		Expect(results).To(HaveLen(2))
		r, ok := results[0].Result.(*cniv04.Result)
		Expect(ok).To(BeTrue())
		Expect(reflect.DeepEqual(r, expectedResult1)).To(BeTrue())
		r, ok = results[1].Result.(*cniv04.Result)
		Expect(ok).To(BeTrue())
		Expect(reflect.DeepEqual(r, expectedResult2)).To(BeTrue())

		resultsStatus, errStatus := ocicni.GetPodNetworkStatus(podNet)
		Expect(errStatus).NotTo(HaveOccurred())
		Expect(resultsStatus).To(HaveLen(2))
		r, ok = resultsStatus[0].Result.(*cniv04.Result)
		Expect(ok).To(BeTrue())
		Expect(reflect.DeepEqual(r, expectedResult1)).To(BeTrue())
		r, ok = resultsStatus[1].Result.(*cniv04.Result)
		Expect(ok).To(BeTrue())
		Expect(reflect.DeepEqual(r, expectedResult2)).To(BeTrue())

		err = ocicni.TearDownPod(podNet)
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.delIndex).To(Equal(len(fake.plugins)))

		Expect(ocicni.Shutdown()).NotTo(HaveOccurred())
	})

	It("correctly issues a GC for the default network", func() {
		_, _, err := writeConfig(tmpDir, "10-network2.conf", "network2", "myplugin", "1.1.0")
		Expect(err).NotTo(HaveOccurred())

		fake := &fakeExec{}

		expectedConf := `
{
	"name": "network2",
	"type": "myplugin",
	"cniVersion": "1.1.0",
	"cni.dev/valid-attachments": [ {"containerID": "1234567890", "ifname": "eth0" }]
}
		`

		fake.addPlugin(nil, expectedConf, nil)

		ocicni, err := initCNI(fake, cacheDir, "network2", tmpDir, true, "/opt/cni/bin")
		Expect(err).NotTo(HaveOccurred())

		podNet := PodNetwork{
			Name:      "pod1",
			Namespace: "namespace1",
			ID:        "1234567890",
			UID:       "9414bd03-b3d3-453e-9d9f-47dcee07958c",
			NetNS:     networkNS.Path(),
		}
		err = ocicni.GC(context.Background(), []*PodNetwork{&podNet})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.gcIndex).To(Equal(len(fake.plugins)))
	})

	It("correctly issues a GC for a speified network", func() {
		_, _, err := writeConfig(tmpDir, "10-network2.conf", "network2", "myplugin", "1.1.0")
		Expect(err).NotTo(HaveOccurred())

		fake := &fakeExec{}

		expectedConf := `
{
	"name": "network2",
	"type": "myplugin",
	"cniVersion": "1.1.0",
	"cni.dev/valid-attachments": [ {"containerID": "1234567890", "ifname": "net1" }]
}
		`

		fake.addPlugin(nil, expectedConf, nil)

		ocicni, err := initCNI(fake, cacheDir, "network2", tmpDir, true, "/opt/cni/bin")
		Expect(err).NotTo(HaveOccurred())

		podNet := PodNetwork{
			Name:      "pod1",
			Namespace: "namespace1",
			ID:        "1234567890",
			UID:       "9414bd03-b3d3-453e-9d9f-47dcee07958c",
			NetNS:     networkNS.Path(),
			Networks: []NetAttachment{
				{Name: "network2", Ifname: "net1"},
			},
		}
		err = ocicni.GC(context.Background(), []*PodNetwork{&podNet})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.gcIndex).To(Equal(len(fake.plugins)))
	})

	Context("when tearing down a pod using cached info", func() {
		const (
			containerID    string = "1234567890"
			netName1       string = "network1"
			ifname1        string = "eth0"
			netName2       string = "network2"
			ifname2        string = "eth1"
			defaultNetName string = "test"
		)
		var (
			fake   *fakeExec
			ocicni CNIPlugin
			podNet PodNetwork
		)

		BeforeEach(func() {
			// Unused default config
			_, _, err := writeConfig(tmpDir, "10-test.conf", defaultNetName, "myplugin", "0.3.1")
			Expect(err).NotTo(HaveOccurred())

			conf1 := fmt.Sprintf(`{
			  "name": "%s",
			  "type": "myplugin",
			  "cniVersion": "0.4.0"
			}`, netName1)
			writeCacheFile(cacheDir, containerID, netName1, ifname1, conf1)

			conf2 := fmt.Sprintf(`{
			  "name": "%s",
			  "type": "myplugin",
			  "cniVersion": "0.4.0"
			}`, netName2)
			writeCacheFile(cacheDir, containerID, netName2, ifname2, conf2)

			fake = &fakeExec{}
			fake.addPlugin([]string{"CNI_IFNAME=" + ifname1}, conf1, nil)
			fake.addPlugin([]string{"CNI_IFNAME=" + ifname2}, conf2, nil)

			ocicni, err = initCNI(fake, cacheDir, defaultNetName, tmpDir, true, "/opt/cni/bin")
			Expect(err).NotTo(HaveOccurred())

			podNet = PodNetwork{
				Name:      "pod1",
				Namespace: "namespace1",
				ID:        containerID,
				UID:       "9414bd03-b3d3-453e-9d9f-47dcee07958c",
				NetNS:     networkNS.Path(),
			}
		})

		AfterEach(func() {
			Expect(ocicni.Shutdown()).NotTo(HaveOccurred())
		})

		It("uses the specified networks", func() {
			podNet.Networks = []NetAttachment{
				{netName1, ifname1},
				{netName2, ifname2},
			}

			err := ocicni.TearDownPod(podNet)
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.delIndex).To(Equal(len(fake.plugins)))
		})

		It("uses the cached networks", func() {
			podNet.Networks = []NetAttachment{}
			err := ocicni.TearDownPod(podNet)
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.delIndex).To(Equal(len(fake.plugins)))
		})
		It("verifies that network operations can be locked for a pod using cached networks", func() {
			podNet.Networks = []NetAttachment{}
			tmp, ok := ocicni.(*cniNetworkPlugin)
			Expect(ok).To(BeTrue())
			Expect(tmp.pods).To(BeEmpty())
			tmp.podLock(&podNet)
			Expect(tmp.pods).To(HaveLen(1))
		})
		It("verifies that network operations can be unlocked for a pod using cached networks", func() {
			podNet.Networks = []NetAttachment{}
			tmp, ok := ocicni.(*cniNetworkPlugin)
			Expect(ok).To(BeTrue())
			tmp.podUnlock(&podNet)
			Expect(tmp.pods).To(BeEmpty())
		})
	})

	It("tears down a pod using specified networks when cached info is missing", func() {
		const (
			containerID    string = "1234567890"
			defaultNetName string = "defaultnet"
			netName1       string = "network1"
			netName2       string = "network2"
		)

		conf1, _, err := writeConfig(tmpDir, fmt.Sprintf("10-%s.conf", netName1), netName1, "myplugin", "0.4.0")
		Expect(err).NotTo(HaveOccurred())

		conf2, _, err := writeConfig(tmpDir, fmt.Sprintf("20-%s.conf", netName2), netName2, "myplugin2", "0.4.0")
		Expect(err).NotTo(HaveOccurred())

		fake := &fakeExec{}
		fake.addPlugin(nil, conf1, nil)
		fake.addPlugin(nil, conf2, nil)

		ocicni, err := initCNI(fake, cacheDir, defaultNetName, tmpDir, true, "/opt/cni/bin")
		Expect(err).NotTo(HaveOccurred())
		defer Expect(ocicni.Shutdown()).NotTo(HaveOccurred())

		podNet := PodNetwork{
			Name:      "pod1",
			Namespace: "namespace1",
			ID:        containerID,
			UID:       "9414bd03-b3d3-453e-9d9f-47dcee07958c",
			NetNS:     networkNS.Path(),
			Networks: []NetAttachment{
				{Name: netName1},
				{Name: netName2},
			},
		}

		err = ocicni.TearDownPod(podNet)
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.delIndex).To(Equal(len(fake.plugins)))
	})
})
