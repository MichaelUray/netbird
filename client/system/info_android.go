package system

import (
	"bytes"
	"context"
	"os/exec"
	"runtime"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/netbirdio/netbird/version"
)

// UpdateStaticInfoAsync is a no-op on Android as there is no static info to update
func UpdateStaticInfoAsync() {
	// do nothing
}

// GetInfo retrieves and parses the system information
func GetInfo(ctx context.Context) *Info {
	kernel := "android"
	osInfo := uname()
	if len(osInfo) == 2 {
		kernel = osInfo[1]
	}

	var kernelVersion string
	if len(osInfo) > 2 {
		kernelVersion = osInfo[2]
	}

	gio := &Info{
		GoOS:               runtime.GOOS,
		Kernel:             kernel,
		Platform:           "unknown",
		OS:                 "Android",
		OSVersion:          osVersion(),
		Hostname:           extractDeviceName(ctx, "android"),
		CPUs:               runtime.NumCPU(),
		NetbirdVersion:     version.NetbirdVersion(),
		UIVersion:          extractUIVersion(ctx),
		KernelVersion:      kernelVersion,
		SystemSerialNumber: serial(),
		SystemProductName:  productModel(),
		SystemManufacturer: productManufacturer(),
	}

	// Fix-A (2026-05-29): populate NetworkAddresses from the
	// Android-specific provider in ctx, if one was attached by
	// client/android/client.go. Without this, meta_network_addresses
	// on the mgmt server is permanently empty for Android peers and
	// every posture-check peer_network_range_check with action=deny
	// fails open. The provider reuses the same IFaceDiscover bridge
	// the ICE-candidate-gather path already consumes.
	gio.NetworkAddresses = androidNetworkAddresses(ctx)

	return gio
}

// androidNetworkAddresses is the Fix-A bridge consumer. Returns nil
// (no addresses, do not break the rest of Info population) if the
// provider is missing or errors — the management server will simply
// see an empty list, which is the pre-fix behaviour. Errors are
// logged at WARN so a misconfigured Android build is visible without
// hard-failing daemon startup.
func androidNetworkAddresses(ctx context.Context) []NetworkAddress {
	p, ok := androidNetworkAddressProviderFromContext(ctx)
	if !ok || p == nil {
		return nil
	}
	raw, err := p.IFaces()
	if err != nil {
		log.Warnf("Fix-A: AndroidNetworkAddressProvider.IFaces() failed: %v (meta_network_addresses will be empty)", err)
		return nil
	}
	addrs := parseAndroidIFacesNetworkAddresses(raw)
	log.Debugf("Fix-A: reported %d Android network address(es) to mgmt: %v", len(addrs), addrs)
	return addrs
}

// checkFileAndProcess checks if the file path exists and if a process is running at that path.
func checkFileAndProcess(paths []string) ([]File, error) {
	return []File{}, nil
}

func serial() string {
	// try to fetch serial ID using different properties
	properties := []string{"ril.serialnumber", "ro.serialno", "ro.boot.serialno", "sys.serialnumber"}
	var value string

	for _, property := range properties {
		value = getprop(property)
		if len(value) > 0 {
			return value
		}
	}

	// unable to get serial ID, fallback to ANDROID_ID
	return androidId()
}

func androidId() string {
	// this is a uniq id defined on first initialization, id will be a new one if user wipes his device
	return run("/system/bin/settings", "get", "secure", "android_id")
}

func productModel() string {
	return getprop("ro.product.model")
}

func productManufacturer() string {
	return getprop("ro.product.manufacturer")
}

func uname() []string {
	res := run("/system/bin/uname", "-a")
	return strings.Split(res, " ")
}

func osVersion() string {
	return getprop("ro.build.version.release")
}

func extractUIVersion(ctx context.Context) string {
	v, ok := ctx.Value(UiVersionCtxKey).(string)
	if !ok {
		return ""
	}
	return v
}

func getprop(arg ...string) string {
	return run("/system/bin/getprop", arg...)
}

func run(name string, arg ...string) string {
	cmd := exec.Command(name, arg...)
	cmd.Stdin = strings.NewReader("some")
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		log.Errorf("getInfo: %s", err)
	}

	return strings.TrimSpace(out.String())
}
