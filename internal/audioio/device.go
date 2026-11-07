package audioio

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/gen2brain/malgo"
)

// matchDeviceIndex resolves want (as accepted by -mic-device/-speaker-device
// / the mic_device/speaker_device config keys) against names, the enumerated
// device names in listing order: a numeric index into names, or a
// case-insensitive substring of a name, first match wins. Pure and
// hardware-free (unlike findDevice/deviceLabel, which each enumerate real
// devices first) so the matching rule itself is unit-testable without real
// device access -- shared by both of those so they always agree on which
// device want refers to.
func matchDeviceIndex(names []string, want string) (int, error) {
	if idx, err := strconv.Atoi(want); err == nil {
		if idx < 0 || idx >= len(names) {
			return 0, fmt.Errorf("device index %d out of range (0-%d)", idx, len(names)-1)
		}
		return idx, nil
	}
	wantLower := strings.ToLower(want)
	for i, name := range names {
		if strings.Contains(strings.ToLower(name), wantLower) {
			return i, nil
		}
	}
	return 0, fmt.Errorf("no device matching %q", want)
}

// deviceNames returns infos' names in order, for matchDeviceIndex.
func deviceNames(infos []malgo.DeviceInfo) []string {
	names := make([]string, len(infos))
	for i := range infos {
		names[i] = infos[i].Name()
	}
	return names
}

// findDevice resolves a device name/index string (as set via -mic-device /
// -speaker-device / the mic_device/speaker_device config keys) to a malgo
// device ID. An empty want returns (nil, nil), meaning "use the system
// default device".
func findDevice(ctx malgo.Context, kind malgo.DeviceType, want string) (*malgo.DeviceID, error) {
	if want == "" {
		return nil, nil
	}
	infos, err := ctx.Devices(kind)
	if err != nil {
		return nil, fmt.Errorf("enumerate devices: %w", err)
	}
	idx, err := matchDeviceIndex(deviceNames(infos), want)
	if err != nil {
		return nil, err
	}
	return &infos[idx].ID, nil
}

// deviceLabel resolves want exactly as findDevice does, but returns a
// human-readable label for logging instead of a device ID -- opening its
// own throwaway context just to enumerate (mirroring listDevices) rather
// than actually opening the device, since the real open (Mic.Stream) happens
// later, so there's no already-open device to describe at the point this
// needs to be logged. An empty want resolves
// to whichever device malgo reports as IsDefault, named explicitly when
// enumerable rather than left as a bare "system default" so -mic-device ""
// and an explicit match both tell you the same thing: which physical device
// will actually be used.
func deviceLabel(kind malgo.DeviceType, want string) (string, error) {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return "", fmt.Errorf("init audio context: %w", err)
	}
	defer func() {
		_ = ctx.Uninit()
		ctx.Free()
	}()
	infos, err := ctx.Devices(kind)
	if err != nil {
		return "", fmt.Errorf("enumerate devices: %w", err)
	}
	names := deviceNames(infos)
	if want == "" {
		for i := range infos {
			if infos[i].IsDefault != 0 {
				return fmt.Sprintf("system default (%s)", names[i]), nil
			}
		}
		return "system default", nil
	}
	idx, err := matchDeviceIndex(names, want)
	if err != nil {
		return "", err
	}
	return names[idx], nil
}

// MicDeviceLabel resolves want (a -mic-device value) to a human-readable
// label naming the microphone device it selects, for startup logging.
func MicDeviceLabel(want string) (string, error) {
	return deviceLabel(malgo.Capture, want)
}

// DeviceInfo describes one capture or playback device for listing purposes.
type DeviceInfo struct {
	Index     int
	Name      string
	IsDefault bool
}

// ListMicDevices returns the available capture (microphone) devices.
func ListMicDevices() ([]DeviceInfo, error) {
	return listDevices(malgo.Capture)
}

func listDevices(kind malgo.DeviceType) ([]DeviceInfo, error) {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, fmt.Errorf("init audio context: %w", err)
	}
	defer func() {
		_ = ctx.Uninit()
		ctx.Free()
	}()

	infos, err := ctx.Devices(kind)
	if err != nil {
		return nil, fmt.Errorf("enumerate devices: %w", err)
	}
	out := make([]DeviceInfo, len(infos))
	for i := range infos {
		out[i] = DeviceInfo{
			Index:     i,
			Name:      infos[i].Name(),
			IsDefault: infos[i].IsDefault != 0,
		}
	}
	return out, nil
}
