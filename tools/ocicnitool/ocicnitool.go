package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/cri-o/ocicni/pkg/ocicni"
)

const (
	EnvBinDir  = "BIN_PATH"
	EnvConfDir = "CONF_PATH"

	DefaultConfDir = "/etc/cni/net.d"
	DefaultBinDir  = "/opt/cni/bin"

	CmdAdd    = "add"
	CmdStatus = "status"
	CmdDel    = "del"
)

func main() {
	if len(os.Args) < 6 {
		usage()
		return
	}

	confdir := os.Getenv(EnvConfDir)
	if confdir == "" {
		confdir = DefaultConfDir
	}
	bindir := os.Getenv(EnvBinDir)
	if bindir == "" {
		bindir = DefaultBinDir
	}

	plugin, err := ocicni.InitCNI(confdir, bindir)
	if err != nil {
		exit(err)
	}

	podNetwork := ocicni.PodNetwork{
		Namespace: os.Args[2],
		Name:      os.Args[3],
		ID:        os.Args[4],
		NetNS:     os.Args[5],
	}

	switch os.Args[1] {
	case CmdAdd:
		exit(plugin.SetUpPod(podNetwork))
	case CmdStatus:
		ip, err := plugin.GetPodNetworkStatus(podNetwork)
		if err != nil {
			exit(err)
		}
		fmt.Fprintf(os.Stdout, "IP: %s\n", ip)
	case CmdDel:
		exit(plugin.TearDownPod(podNetwork))
	}
}

func usage() {
	exe := filepath.Base(os.Args[0])

	fmt.Fprintf(os.Stderr, "%s: Add or remove network interfaces from a network namespace\n", exe)
	fmt.Fprintf(os.Stderr, "  %s %s <pod_namespace> <pod_name> <pod_id> <netns>\n", exe, CmdAdd)
	fmt.Fprintf(os.Stderr, "  %s %s <pod_namespace> <pod_name> <pod_id> <netns>\n", exe, CmdStatus)
	fmt.Fprintf(os.Stderr, "  %s %s <pod_namespace> <pod_name> <pod_id> <netns>\n", exe, CmdDel)
	os.Exit(1)
}

func exit(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}
