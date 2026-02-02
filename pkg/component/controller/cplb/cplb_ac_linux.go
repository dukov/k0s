package cplb

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"text/template"
	"time"

	"github.com/k0sproject/k0s/internal/pkg/file"
	"github.com/k0sproject/k0s/internal/pkg/users"
	k0sAPI "github.com/k0sproject/k0s/pkg/apis/k0s/v1beta1"
	"github.com/k0sproject/k0s/pkg/assets"
	"github.com/k0sproject/k0s/pkg/config"
	"github.com/k0sproject/k0s/pkg/constant"
	"github.com/k0sproject/k0s/pkg/supervisor"
	"github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
)

const (
	// birdResponseCodeSuccess is the success response code in bird CLI protocol (first 4 bytes of last line).
	birdResponseCodeSuccess = "0003"
	// configureCommand is the bird CLI command to re-read the configuration.
	configureCommand = "configure\n"
)

type Anycast struct {
	K0sVars         *config.CfgVars
	Config          *k0sAPI.AnycastSpec
	DetailedLogging bool
	LogConfig       bool
	APIPort         int
	APIAddress      string
	KubeConfigPath  string

	log            *logrus.Entry
	uid            int
	executablePath string
	configFilePath string
	socketFilePath string
	reconciler     *CPLBReconciler
	updateCh       chan struct{}
	reconcilerDone chan struct{}
	birdConfig     *birdConfig
	supervisor     *supervisor.Supervisor
}

func (a *Anycast) Init(ctx context.Context) error {
	if a.Config == nil {
		return nil
	}
	a.log = logrus.WithField("component", "CPLB")

	var err error
	a.uid, err = users.LookupUID(constant.BirdUser)
	if err != nil {
		err = fmt.Errorf("failed to lookup UID for %q: %w", constant.BirdUser, err)
		a.uid = users.RootUID
		a.log.WithError(err).Warn("Running bird as root")
	}

	a.configFilePath = filepath.Join(a.K0sVars.RunDir, "bird.conf")
	a.socketFilePath = filepath.Join(a.K0sVars.RunDir, "bird.ctl")
	a.executablePath, err = assets.StageExecutable(a.K0sVars.BinDir, "bird")
	return err
}

func (a *Anycast) Start(ctx context.Context) error {
	if a.Config == nil || len(a.Config.BGP) == 0 {
		a.log.Warn("No Anycast configuration defined, skipping start")
		return nil
	}

	// TODO: addEnsureIP address function whic assigns ip addres to lo
	if err := a.ensureLinkAddresses("lo", []string{"127.0.0.1/8", "::1/128", a.Config.AnycastIP + "/32"}); err != nil {
		return fmt.Errorf("failed to ensure link addresses: %w", err)
	}

	a.log.Info("Starting CPLB reconciler")
	updateCh := make(chan struct{}, 1)
	a.reconciler = NewCPLBReconciler(a.KubeConfigPath, a.APIPort, updateCh)
	a.updateCh = updateCh
	if err := a.reconciler.Start(); err != nil {
		return fmt.Errorf("failed to start CPLB reconciler: %w", err)
	}

	a.birdConfig = a.buildBirdConfig(false)

	templ := template.Must(template.New("bird").Parse(birdConfigTemplate))
	if err := a.generateTemplate(templ, a.configFilePath); err != nil {
		return fmt.Errorf("failed to generate bird template: %w", err)
	}

	args := []string{
		"-f",
		"-c",
		a.configFilePath,
		"-s",
		a.socketFilePath,
	}

	if a.uid != users.RootUID {
		args = append(args, "-u", constant.BirdUser)
	}

	if a.DetailedLogging {
		args = append(args, "-d")
	}

	a.log.Infoln("Starting bird")
	a.supervisor = &supervisor.Supervisor{
		Name:    "bird",
		BinPath: a.executablePath,
		Args:    args,
		RunDir:  a.K0sVars.RunDir,
		DataDir: a.K0sVars.DataDir,
		UID:     a.uid,
	}

	if a.reconciler != nil {
		reconcilerDone := make(chan struct{})
		a.reconcilerDone = reconcilerDone
		go func() {
			defer close(reconcilerDone)
			if err := a.watchReconcilerUpdates(ctx); err != nil {
				a.log.WithError(err).Error("failed to watch reconciler updates")
			}
		}()
	}

	return a.supervisor.Supervise(ctx)
}

func (a *Anycast) Stop() error {
	if a.reconciler != nil {
		a.log.Info("Stopping cplb-reconciler")
		a.reconciler.Stop()
		close(a.updateCh)
		<-a.reconcilerDone
	}

	a.log.Info("Stopping bird")
	if err := a.supervisor.Stop(); err != nil {
		a.log.WithError(err).Error("Failed to stop executable")
	}

	a.log.Info("Deleting dummy interface")
	link, err := netlink.LinkByName(dummyLinkName)
	if err != nil {
		return err
	}

	return netlink.AddrDel(link, &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   net.ParseIP(a.Config.AnycastIP),
			Mask: net.CIDRMask(32, 32),
		},
	})
}

// birdConfig holds data for the bird config template.
type birdConfig struct {
	RouterID  string
	AnycastIP string
	BGP       []birdBGPEntry
}

// birdBGPEntry is a BGP section in the bird config with Export flag.
type birdBGPEntry struct {
	Name      string
	ASNumber  int
	Neighbors []string
	Export    bool
}

// buildBirdConfig builds bird config from Anycast spec. export controls whether
// the anycast IP is exported via BGP (enabled only when this node's APIAddress
// is in the healthy endpoint set).
func (a *Anycast) buildBirdConfig(export bool) *birdConfig {
	routerID := a.Config.RouterID
	if routerID == "" {
		routerID = a.Config.AnycastIP
	}
	bgp := make([]birdBGPEntry, len(a.Config.BGP))
	for i := range a.Config.BGP {
		bgp[i] = birdBGPEntry{
			Name:      a.Config.BGP[i].Name,
			ASNumber:  a.Config.BGP[i].ASNumber,
			Neighbors: a.Config.BGP[i].Neighbors,
			Export:    export,
		}
	}
	return &birdConfig{
		RouterID:  routerID,
		AnycastIP: a.Config.AnycastIP,
		BGP:       bgp,
	}
}

func (a *Anycast) generateTemplate(templ *template.Template, path string) error {
	if err := file.AtomicWithTarget(path).
		WithPermissions(0400).
		WithOwner(a.uid).
		Do(func(unbuffered file.AtomicWriter) error {
			w := bufio.NewWriter(unbuffered)
			if err := templ.Execute(w, a.birdConfig); err != nil {
				return err
			}
			return w.Flush()
		}); err != nil {
		return fmt.Errorf("failed to write bird config file: %w", err)
	}

	return nil
}

// TODO move this to utils to use in Keepalived and in Anycast
func (a *Anycast) ensureLinkAddresses(linkName string, expectedAddresses []string) error {
	link, err := netlink.LinkByName(linkName)
	if err != nil {
		return fmt.Errorf("failed to get link by name %s: %w", linkName, err)
	}

	linkAddrs, strAddrs, err := a.getLinkAddresses(link)
	if err != nil {
		return fmt.Errorf("failed to get addresses for link %s: %w", linkName, err)
	}

	// Remove unexpected addresses
	for i := range linkAddrs {
		strAddr := strAddrs[i]
		linkAddr := linkAddrs[i]
		if !slices.Contains(expectedAddresses, strAddrs[i]) {
			a.log.Infof("Deleting address %s from link %s", strAddr, linkName)
			if err = netlink.AddrDel(link, &linkAddr); err != nil {
				return fmt.Errorf("failed to delete address %s from link %s: %w", linkAddr.IPNet.String(), linkName, err)
			}
		}
	}

	// Add missing expected addresses
	for _, addr := range expectedAddresses {
		if !slices.Contains(strAddrs, addr) {
			if err = a.setLinkIP(addr, linkName, link); err != nil {
				return fmt.Errorf("failed to add address %s to link %s: %w", addr, linkName, err)
			}
		}
	}

	return nil
}

// TODO move this to utils to use in Keepalived and in Anycast
func (*Anycast) getLinkAddresses(link netlink.Link) ([]netlink.Addr, []string, error) {
	linkAddrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list addresses for link %s: %w", link.Attrs().Name, err)
	}

	strAddrs := make([]string, len(linkAddrs))
	for i, addr := range linkAddrs {
		strAddrs[i] = addr.IPNet.String()
	}
	return linkAddrs, strAddrs, nil
}

func (a *Anycast) setLinkIP(addr string, linkName string, link netlink.Link) error {
	ipAddr, _, err := net.ParseCIDR(addr)
	if err != nil {
		return fmt.Errorf("failed to parse CIDR %s: %w", addr, err)
	}

	var mask net.IPMask
	if ipAddr.To4() != nil {
		mask = net.CIDRMask(32, 32)
	} else {
		mask = net.CIDRMask(128, 128)
	}

	linkAddr := &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   ipAddr,
			Mask: mask,
		},
	}

	a.log.Infof("Adding address %s to link %s", addr, linkName)
	if err := netlink.AddrAdd(link, linkAddr); err != nil {
		return fmt.Errorf("failed to add address %s to link %s: %w", addr, linkName, err)
	}
	return nil
}

func (a *Anycast) watchReconcilerUpdates(ctx context.Context) error {
	// Wait for the supervisor to start bird before watching for endpoint changes
	// TODO this should be defined as a method of Supervisor
	process := a.supervisor.GetProcess()
	for i := 0; process == nil; i++ {
		if i > 3 {
			a.log.Error("failed to start bird, supervisor process is nil")
			return nil
		}
		a.log.Info("Waiting for bird to start")
		time.Sleep(5 * time.Second)
		process = a.supervisor.GetProcess()
	}

	a.log.Info("started watching cplb-reconciler updates")
	templ := template.Must(template.New("bird").Parse(birdConfigTemplate))
	birdSocketConn, err := NewUnixSocketConn(ctx, a.socketFilePath)
	if err != nil {
		return fmt.Errorf("failed to create bird socket connection: %w", err)
	}
	defer birdSocketConn.Close()
	for range a.updateCh {
		endpointIPs := a.reconciler.GetIPs()
		exportAnycast := slices.Contains(endpointIPs, a.APIAddress)
		a.log.Infof("cplb-reconciler update, endpoint IPs %v, APIAddress %s, export anycast: %v", endpointIPs, a.APIAddress, exportAnycast)
		a.birdConfig = a.buildBirdConfig(exportAnycast)
		if err := a.generateTemplate(templ, a.configFilePath); err != nil {
			a.log.Errorf("failed to generate bird template: %v", err)
			continue
		}
		resp, err := birdSocketConn.Write([]byte(configureCommand))
		if err != nil {
			a.log.Errorf("failed to send configure command to bird: %v", err)
			continue
		}
		if code := birdResponseCode(resp); code != birdResponseCodeSuccess {
			a.log.Errorf("bird configure command failed, response code %q (expected %q)", code, birdResponseCodeSuccess)
			continue
		}
	}
	a.log.Info("stopped watching cplb-reconciler updates")
	return nil
}

const birdConfigTemplate = `
router id {{ .RouterID }};

filter export_single_subnet {
    if net = {{ .AnycastIP }}/32 then accept;
    reject;
}

protocol device {
        scan time 60;
}

protocol direct direct1 {
    interface "lo";
}

{{- range $i, $bgp := .BGP }}
protocol bgp {{ $bgp.Name }} {
    local as {{ $bgp.ASNumber }};
    disabled no;
{{- range $bgp.Neighbors }}
    neighbor {{ . }} as {{ $bgp.ASNumber }};
{{- end }}
    ipv4 {
        import none;
	    {{- if $bgp.Export }}
        export filter export_single_subnet;
	    {{- else }}
        export none;
	    {{- end }}
    };
}
{{- end }}
`
