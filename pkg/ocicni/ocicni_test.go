package ocicni

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/cni/pkg/version"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func writeConfig(dir, fileName, netName, plugin string) (string, string, error) {
	confPath := filepath.Join(dir, fileName)
	conf := fmt.Sprintf(`{
	"name": "%s",
	"type": "%s",
	"cniVersion": "0.3.1"
}`, netName, plugin)
	return conf, confPath, ioutil.WriteFile(confPath, []byte(conf), 0644)
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
	plugins  []*fakePlugin
}

func (f *fakeExec) addLoopback() {
	f.plugins = append(f.plugins, &fakePlugin{
		expectedConf: `{
        "cniVersion": "0.2.0",
        "name": "cni-loopback",
        "type": "loopback"
}`,
		result: &current.Result{
			CNIVersion: "0.2.0",
		},
	})
}

func (f *fakeExec) addPlugin(expectedEnv []string, expectedConf string, result *current.Result, err error) {
	f.plugins = append(f.plugins, &fakePlugin{
		expectedEnv:  expectedEnv,
		expectedConf: expectedConf,
		result:       result,
		err:          err,
	})
}

func matchArray(a1, a2 []string) {
	Expect(len(a1)).To(Equal(len(a2)))
	for _, e1 := range a1 {
		found := ""
		for _, e2 := range a2 {
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
	return "", fmt.Errorf("failed to find CNI_COMMAND")
}

func (f *fakeExec) ExecPlugin(pluginPath string, stdinData []byte, environ []string) ([]byte, error) {
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
		// +1 to skip loopback since it isn't run on DEL
		index = f.delIndex + 1
		f.delIndex++
	default:
		// Should never be reached
		Expect(false).To(BeTrue())
	}
	plugin := f.plugins[index]

	GinkgoT().Logf("[%s %d] exec plugin %q found %+v", cmd, index, pluginPath, plugin)

	if plugin.expectedConf != "" {
		Expect(string(stdinData)).To(MatchJSON(plugin.expectedConf))
	}
	if len(plugin.expectedEnv) > 0 {
		matchArray(environ, plugin.expectedEnv)
	}

	if plugin.err != nil {
		return nil, plugin.err
	}

	resultJSON, err := json.Marshal(plugin.result)
	Expect(err).NotTo(HaveOccurred())
	return resultJSON, nil
}

func (f *fakeExec) FindInPath(plugin string, paths []string) (string, error) {
	Expect(len(paths)).To(BeNumerically(">", 0))
	return filepath.Join(paths[0], plugin), nil
}

func ensureCIDR(cidr string) *net.IPNet {
	ip, net, err := net.ParseCIDR(cidr)
	Expect(err).NotTo(HaveOccurred())
	net.IP = ip
	return net
}

var _ = Describe("ocicni operations", func() {
	var (
		tmpDir   string
		cacheDir string
	)

	BeforeEach(func() {
		var err error
		tmpDir, err = ioutil.TempDir("", "ocicni_tmp")
		Expect(err).NotTo(HaveOccurred())
		cacheDir, err = ioutil.TempDir("", "ocicni_cache")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		err := os.RemoveAll(tmpDir)
		Expect(err).NotTo(HaveOccurred())
		err = os.RemoveAll(cacheDir)
		Expect(err).NotTo(HaveOccurred())
	})

	It("finds an existing default network configuration", func() {
		_, _, err := writeConfig(tmpDir, "5-notdefault.conf", "notdefault", "myplugin")
		Expect(err).NotTo(HaveOccurred())
		_, _, err = writeConfig(tmpDir, "10-test.conf", "test", "myplugin")
		Expect(err).NotTo(HaveOccurred())

		ocicni, err := InitCNI("test", tmpDir, "/opt/cni/bin")
		Expect(err).NotTo(HaveOccurred())
		Expect(ocicni.Status()).NotTo(HaveOccurred())

		// Ensure the default network is the one we expect
		tmp := ocicni.(*cniNetworkPlugin)
		net := tmp.getDefaultNetwork()
		Expect(net.name).To(Equal("test"))
		Expect(len(net.NetworkConfig.Plugins)).To(BeNumerically(">", 0))
		Expect(net.NetworkConfig.Plugins[0].Network.Type).To(Equal("myplugin"))

		ocicni.Shutdown()
	})

	It("finds an asynchronously written default network configuration", func() {
		ocicni, err := InitCNI("test", tmpDir, "/opt/cni/bin")
		Expect(err).NotTo(HaveOccurred())

		// Writing a config that doesn't match the default network
		_, _, err = writeConfig(tmpDir, "5-notdefault.conf", "notdefault", "myplugin")
		Expect(err).NotTo(HaveOccurred())
		Consistently(ocicni.Status, 5).Should(HaveOccurred())

		_, _, err = writeConfig(tmpDir, "10-test.conf", "test", "myplugin")
		Expect(err).NotTo(HaveOccurred())
		Eventually(ocicni.Status, 5).Should(Succeed())

		tmp := ocicni.(*cniNetworkPlugin)
		net := tmp.getDefaultNetwork()
		Expect(net.name).To(Equal("test"))
		Expect(len(net.NetworkConfig.Plugins)).To(BeNumerically(">", 0))
		Expect(net.NetworkConfig.Plugins[0].Network.Type).To(Equal("myplugin"))

		ocicni.Shutdown()
	})

	It("finds and refinds an asynchronously written default network configuration", func() {
		ocicni, err := InitCNI("test", tmpDir, "/opt/cni/bin")
		Expect(err).NotTo(HaveOccurred())

		// Write the default network config
		_, confPath, err := writeConfig(tmpDir, "10-test.conf", "test", "myplugin")
		Expect(err).NotTo(HaveOccurred())
		Eventually(ocicni.Status, 5).Should(Succeed())

		tmp := ocicni.(*cniNetworkPlugin)
		net := tmp.getDefaultNetwork()
		Expect(net.name).To(Equal("test"))
		Expect(len(net.NetworkConfig.Plugins)).To(BeNumerically(">", 0))
		Expect(net.NetworkConfig.Plugins[0].Network.Type).To(Equal("myplugin"))

		// Delete the default network config, ensure ocicni begins to
		// return a status error
		err = os.Remove(confPath)
		Expect(err).NotTo(HaveOccurred())
		Eventually(ocicni.Status, 5).Should(HaveOccurred())

		// Write the default network config again and wait for status
		// to be OK
		_, _, err = writeConfig(tmpDir, "10-test.conf", "test", "myplugin")
		Expect(err).NotTo(HaveOccurred())
		Eventually(ocicni.Status, 5).Should(Succeed())

		ocicni.Shutdown()
	})

	It("finds an the asciibetically first network configuration as default if given no default network name", func() {
		ocicni, err := InitCNI("", tmpDir, "/opt/cni/bin")
		Expect(err).NotTo(HaveOccurred())

		_, _, err = writeConfig(tmpDir, "10-test.conf", "test", "myplugin")
		Expect(err).NotTo(HaveOccurred())
		_, _, err = writeConfig(tmpDir, "5-notdefault.conf", "notdefault", "myplugin")
		Expect(err).NotTo(HaveOccurred())

		Eventually(ocicni.Status, 5).Should(Succeed())

		tmp := ocicni.(*cniNetworkPlugin)
		net := tmp.getDefaultNetwork()
		Expect(net.name).To(Equal("test"))
		Expect(len(net.NetworkConfig.Plugins)).To(BeNumerically(">", 0))
		Expect(net.NetworkConfig.Plugins[0].Network.Type).To(Equal("myplugin"))

		ocicni.Shutdown()
	})

	It("returns correct default network from loadNetworks()", func() {
		// Writing a config that doesn't match the default network
		_, _, err := writeConfig(tmpDir, "5-network1.conf", "network1", "myplugin")
		Expect(err).NotTo(HaveOccurred())
		_, _, err = writeConfig(tmpDir, "10-network2.conf", "network2", "myplugin")
		Expect(err).NotTo(HaveOccurred())
		_, _, err = writeConfig(tmpDir, "30-network3.conf", "network3", "myplugin")
		Expect(err).NotTo(HaveOccurred())
		_, _, err = writeConfig(tmpDir, "afdsfdsafdsa-network3.conf", "network4", "myplugin")
		Expect(err).NotTo(HaveOccurred())

		netMap, defname, err := loadNetworks(nil, tmpDir, []string{"/opt/cni/bin"})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(netMap)).To(Equal(4))
		// filenames are sorted asciibetically
		Expect(defname).To(Equal("network2"))
	})

	It("returns no error from loadNetworks() when no config files exist", func() {
		netMap, defname, err := loadNetworks(nil, tmpDir, []string{"/opt/cni/bin"})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(netMap)).To(Equal(0))
		// filenames are sorted asciibetically
		Expect(defname).To(Equal(""))
	})

	It("ignores subsequent duplicate network names in loadNetworks()", func() {
		// Writing a config that doesn't match the default network
		_, _, err := writeConfig(tmpDir, "10-network2.conf", "network2", "myplugin")
		Expect(err).NotTo(HaveOccurred())
		_, _, err = writeConfig(tmpDir, "30-network3.conf", "network3", "myplugin")
		Expect(err).NotTo(HaveOccurred())
		_, _, err = writeConfig(tmpDir, "5-network1.conf", "network2", "myplugin2")
		Expect(err).NotTo(HaveOccurred())

		netMap, _, err := loadNetworks(nil, tmpDir, []string{"/opt/cni/bin"})
		Expect(err).NotTo(HaveOccurred())

		// We expect the type=myplugin network to be ignored since it
		// was read earlier than the type=myplugin2 network with the same name
		Expect(len(netMap)).To(Equal(2))
		net, ok := netMap["network2"]
		Expect(ok).To(BeTrue())
		Expect(net.NetworkConfig.Plugins[0].Network.Type).To(Equal("myplugin2"))
	})

	It("sets up and tears down a pod using the default network", func() {
		conf, _, err := writeConfig(tmpDir, "10-network2.conf", "network2", "myplugin")
		Expect(err).NotTo(HaveOccurred())

		fake := &fakeExec{}
		fake.addLoopback()
		expectedResult := &current.Result{
			CNIVersion: "0.3.1",
			Interfaces: []*current.Interface{
				{
					Name:    "eth0",
					Mac:     "01:23:45:67:89:01",
					Sandbox: "/foo/bar/netns",
				},
			},
			IPs: []*current.IPConfig{
				{
					Interface: current.Int(0),
					Version:   "4",
					Address:   *ensureCIDR("1.1.1.2/24"),
				},
			},
		}
		fake.addPlugin(nil, conf, expectedResult, nil)

		ocicni, err := initCNI(fake, cacheDir, "network2", tmpDir, "/opt/cni/bin")
		Expect(err).NotTo(HaveOccurred())

		podNet := PodNetwork{
			Name:      "pod1",
			Namespace: "namespace1",
			ID:        "1234567890",
			NetNS:     "/foo/bar/netns",
		}
		results, err := ocicni.SetUpPod(podNet)
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.addIndex).To(Equal(len(fake.plugins)))
		Expect(len(results)).To(Equal(1))
		r := results[0].(*current.Result)
		Expect(reflect.DeepEqual(r, expectedResult)).To(BeTrue())

		err = ocicni.TearDownPod(podNet)
		Expect(err).NotTo(HaveOccurred())
		// -1 because loopback doesn't get torn down
		Expect(fake.delIndex).To(Equal(len(fake.plugins) - 1))

		ocicni.Shutdown()
	})

	It("sets up and tears down a pod using specified networks", func() {
		_, _, err := writeConfig(tmpDir, "10-network2.conf", "network2", "myplugin")
		Expect(err).NotTo(HaveOccurred())

		conf1, _, err := writeConfig(tmpDir, "20-network3.conf", "network3", "myplugin")
		Expect(err).NotTo(HaveOccurred())
		conf2, _, err := writeConfig(tmpDir, "30-network4.conf", "network4", "myplugin")
		Expect(err).NotTo(HaveOccurred())

		fake := &fakeExec{}
		fake.addLoopback()
		expectedResult1 := &current.Result{
			CNIVersion: "0.3.1",
			Interfaces: []*current.Interface{
				{
					Name:    "eth0",
					Mac:     "01:23:45:67:89:01",
					Sandbox: "/foo/bar/netns",
				},
			},
			IPs: []*current.IPConfig{
				{
					Interface: current.Int(0),
					Version:   "4",
					Address:   *ensureCIDR("1.1.1.2/24"),
				},
			},
		}
		fake.addPlugin(nil, conf1, expectedResult1, nil)

		expectedResult2 := &current.Result{
			CNIVersion: "0.3.1",
			Interfaces: []*current.Interface{
				{
					Name:    "eth1",
					Mac:     "01:23:45:67:89:02",
					Sandbox: "/foo/bar/netns",
				},
			},
			IPs: []*current.IPConfig{
				{
					Interface: current.Int(0),
					Version:   "4",
					Address:   *ensureCIDR("1.1.1.3/24"),
				},
			},
		}
		fake.addPlugin(nil, conf2, expectedResult2, nil)

		ocicni, err := initCNI(fake, cacheDir, "network2", tmpDir, "/opt/cni/bin")
		Expect(err).NotTo(HaveOccurred())

		podNet := PodNetwork{
			Name:      "pod1",
			Namespace: "namespace1",
			ID:        "1234567890",
			NetNS:     "/foo/bar/netns",
			Networks:  []string{"network3", "network4"},
		}
		results, err := ocicni.SetUpPod(podNet)
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.addIndex).To(Equal(len(fake.plugins)))
		Expect(len(results)).To(Equal(2))
		r := results[0].(*current.Result)
		Expect(reflect.DeepEqual(r, expectedResult1)).To(BeTrue())
		r = results[1].(*current.Result)
		Expect(reflect.DeepEqual(r, expectedResult2)).To(BeTrue())

		err = ocicni.TearDownPod(podNet)
		Expect(err).NotTo(HaveOccurred())
		// -1 because loopback doesn't get torn down
		Expect(fake.delIndex).To(Equal(len(fake.plugins) - 1))

		ocicni.Shutdown()
	})
})
