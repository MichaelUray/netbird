package system

import (
	"context"
	"net/netip"
	"strings"

	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/metadata"

	"github.com/netbirdio/netbird/shared/management/proto"
)

// AndroidNetworkAddressProvider is the bridge through which the Android
// daemon surfaces its local network-interface inventory to the
// system.GetInfo() population path. Without this bridge the embedded
// Go runtime's net.Interfaces() inside the VpnService sandbox returns
// only the tun device (no wlan0 / rmnet*), and the management server's
// posture-check `peer_network_range_check` cannot evaluate any real
// LAN prefix → `action=deny` rules are silently bypassed (Fix-A scope).
//
// Reuses the existing IFaceDiscover.IFaces() format already maintained
// by netbird-android/tool/.../IFaceDiscover.java for the ICE-candidate
// gather path — same wire-format, same Java callsite, just a new
// reader on the Go side. Fix-A Codex recommendation (2026-05-29).
type AndroidNetworkAddressProvider interface {
	// IFaces returns the same newline-separated, pipe-delimited
	// interface description that ICE consumes:
	//   name idx mtu up bcast loop p2p mcast|<cidr1> <cidr2> ...
	IFaces() (string, error)
}

// androidNetworkAddressProviderCtxKey is the context-bag slot for
// AndroidNetworkAddressProvider. Private so callers must use the
// WithAndroidNetworkAddressProvider / androidNetworkAddressProviderFromContext
// helpers.
type androidNetworkAddressProviderCtxKey struct{}

// WithAndroidNetworkAddressProvider attaches the bridge to ctx so that
// info_android.go's GetInfo() can read it without taking a direct
// dependency on the netbird/client/android package (which would create
// an import cycle for non-android builds).
func WithAndroidNetworkAddressProvider(ctx context.Context, p AndroidNetworkAddressProvider) context.Context {
	if p == nil {
		return ctx
	}
	return context.WithValue(ctx, androidNetworkAddressProviderCtxKey{}, p)
}

// androidNetworkAddressProviderFromContext is the matched reader. Returns
// nil + false when no provider has been attached (desktop / iOS builds,
// or Android tests that don't wire one up).
func androidNetworkAddressProviderFromContext(ctx context.Context) (AndroidNetworkAddressProvider, bool) {
	if ctx == nil {
		return nil, false
	}
	v := ctx.Value(androidNetworkAddressProviderCtxKey{})
	if v == nil {
		return nil, false
	}
	p, ok := v.(AndroidNetworkAddressProvider)
	if !ok || p == nil {
		return nil, false
	}
	return p, true
}

// DeviceNameCtxKey context key for device name
const DeviceNameCtxKey = "deviceName"

// OsVersionCtxKey context key for operating system version
const OsVersionCtxKey = "OsVersion"

// OsNameCtxKey context key for operating system name
const OsNameCtxKey = "OsName"

// UiVersionCtxKey context key for user UI version
const UiVersionCtxKey = "user-agent"

type NetworkAddress struct {
	NetIP netip.Prefix
	Mac   string
}

type Environment struct {
	Cloud    string
	Platform string
}

type File struct {
	Path             string
	Exist            bool
	ProcessIsRunning bool
}

// Info is an object that contains machine information
// Most of the code is taken from https://github.com/matishsiao/goInfo
type Info struct {
	GoOS               string
	Kernel             string
	Platform           string
	OS                 string
	OSVersion          string
	Hostname           string
	CPUs               int
	NetbirdVersion     string
	UIVersion          string
	KernelVersion      string
	NetworkAddresses   []NetworkAddress
	SystemSerialNumber string
	SystemProductName  string
	SystemManufacturer string
	Environment        Environment
	Files              []File // for posture checks

	RosenpassEnabled    bool
	RosenpassPermissive bool
	ServerSSHAllowed    bool

	DisableClientRoutes bool
	DisableServerRoutes bool
	DisableDNS          bool
	DisableFirewall     bool
	BlockLANAccess      bool
	BlockInbound        bool

	LazyConnectionEnabled bool

	EnableSSHRoot                 bool
	EnableSSHSFTP                 bool
	EnableSSHLocalPortForwarding  bool
	EnableSSHRemotePortForwarding bool
	DisableSSHAuth                bool
}

func (i *Info) SetFlags(
	rosenpassEnabled, rosenpassPermissive bool,
	serverSSHAllowed *bool,
	disableClientRoutes, disableServerRoutes,
	disableDNS, disableFirewall, blockLANAccess, blockInbound, lazyConnectionEnabled bool,
	enableSSHRoot, enableSSHSFTP, enableSSHLocalPortForwarding, enableSSHRemotePortForwarding *bool,
	disableSSHAuth *bool,
) {
	i.RosenpassEnabled = rosenpassEnabled
	i.RosenpassPermissive = rosenpassPermissive
	if serverSSHAllowed != nil {
		i.ServerSSHAllowed = *serverSSHAllowed
	}

	i.DisableClientRoutes = disableClientRoutes
	i.DisableServerRoutes = disableServerRoutes
	i.DisableDNS = disableDNS
	i.DisableFirewall = disableFirewall
	i.BlockLANAccess = blockLANAccess
	i.BlockInbound = blockInbound

	i.LazyConnectionEnabled = lazyConnectionEnabled

	if enableSSHRoot != nil {
		i.EnableSSHRoot = *enableSSHRoot
	}
	if enableSSHSFTP != nil {
		i.EnableSSHSFTP = *enableSSHSFTP
	}
	if enableSSHLocalPortForwarding != nil {
		i.EnableSSHLocalPortForwarding = *enableSSHLocalPortForwarding
	}
	if enableSSHRemotePortForwarding != nil {
		i.EnableSSHRemotePortForwarding = *enableSSHRemotePortForwarding
	}
	if disableSSHAuth != nil {
		i.DisableSSHAuth = *disableSSHAuth
	}
}

// extractUserAgent extracts Netbird's agent (client) name and version from the outgoing context
func extractUserAgent(ctx context.Context) string {
	md, hasMeta := metadata.FromOutgoingContext(ctx)
	if hasMeta {
		agent, ok := md["user-agent"]
		if ok {
			nbAgent := strings.Split(agent[0], " ")[0]
			if strings.HasPrefix(nbAgent, "netbird") {
				return nbAgent
			}
			return ""
		}
	}
	return ""
}

// extractDeviceName extracts device name from context or returns the default system name
func extractDeviceName(ctx context.Context, defaultName string) string {
	v, ok := ctx.Value(DeviceNameCtxKey).(string)
	if !ok {
		return defaultName
	}
	return v
}

// GetInfoWithChecks retrieves and parses the system information with applied checks.
func GetInfoWithChecks(ctx context.Context, checks []*proto.Checks) (*Info, error) {
	log.Debugf("gathering system information with checks: %d", len(checks))
	processCheckPaths := make([]string, 0)
	for _, check := range checks {
		processCheckPaths = append(processCheckPaths, check.GetFiles()...)
	}

	files, err := checkFileAndProcess(processCheckPaths)
	if err != nil {
		return nil, err
	}
	log.Debugf("gathering process check information completed")

	info := GetInfo(ctx)
	info.Files = files

	log.Debugf("all system information gathered successfully")
	return info, nil
}
