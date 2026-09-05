//go:build windows && !386

package gateway

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/windshare/windshare/connectivity/networkstate"
	r "github.com/windshare/windshare/connectivity/reachability"
)

const dhcpPCPv4Option = 158
const dhcpBufferSize = 4096
const dhcpMaxBuffer = 64 * 1024
const dhcpSynchronous = 2

type dhcpParameter struct {
	Flags    uint32
	OptionID uint32
	Vendor   int32
	Data     *byte
	Size     uint32
}
type dhcpParameters struct {
	Count      uint32
	Parameters *dhcpParameter
}

var dhcpLibrary = windows.NewLazySystemDLL("dhcpcsvc.dll")
var dhcpInitialize = dhcpLibrary.NewProc("DhcpCApiInitialize")
var dhcpCleanup = dhcpLibrary.NewProc("DhcpCApiCleanup")
var dhcpRequest = dhcpLibrary.NewProc("DhcpRequestParams")

// The documented API can block for two minutes when its cache misses. Only
// Discovery's process-capacity worker invokes it, and cancelled generations can
// never publish its eventual result. No persistent DHCP request is installed.
func (SystemDHCPSource) Acquire(ctx context.Context, address networkstate.Address) (DHCPOptions, error) {
	if err := ctx.Err(); err != nil {
		return DHCPOptions{}, err
	}
	// This API does not expose repeated DHCPv6 option boundaries. Flattening them
	// would change server groups, so native v6 acquisition is explicitly unavailable.
	if !address.IP.Is4() || address.AdapterID == "" {
		return DHCPOptions{}, r.ErrUnavailable
	}
	name, err := windows.UTF16PtrFromString(address.AdapterID)
	if err != nil {
		return DHCPOptions{}, err
	}
	if err = dhcpInitialize.Find(); err != nil {
		return DHCPOptions{}, fmt.Errorf("%w: %w", r.ErrUnavailable, err)
	}
	if err = dhcpRequest.Find(); err != nil {
		return DHCPOptions{}, fmt.Errorf("%w: %w", r.ErrUnavailable, err)
	}
	var version uint32
	status, _, _ := dhcpInitialize.Call(uintptr(unsafe.Pointer(&version)))
	if status != 0 {
		return DHCPOptions{}, fmt.Errorf("%w: DHCP initialization %d", r.ErrUnavailable, status)
	}
	defer func() { _, _, _ = dhcpCleanup.Call() }()
	size := uint32(dhcpBufferSize)
	for range 2 {
		buffer := make([]byte, size)
		parameter := dhcpParameter{OptionID: dhcpPCPv4Option}
		receive := dhcpParameters{Count: 1, Parameters: &parameter}
		send := dhcpParameters{}
		status, _, _ = dhcpRequest.Call(dhcpSynchronous, 0, uintptr(unsafe.Pointer(name)), 0, uintptr(unsafe.Pointer(&send)), uintptr(unsafe.Pointer(&receive)), uintptr(unsafe.Pointer(&buffer[0])), uintptr(unsafe.Pointer(&size)), 0)
		runtime.KeepAlive(name)
		runtime.KeepAlive(send)
		runtime.KeepAlive(receive)
		if err := ctx.Err(); err != nil {
			return DHCPOptions{}, err
		}
		if status == uintptr(windows.ERROR_MORE_DATA) && size <= dhcpMaxBuffer {
			continue
		}
		if status != 0 {
			return DHCPOptions{}, fmt.Errorf("%w: DHCP option request %d", r.ErrUnavailable, status)
		}
		if parameter.Size == 0 {
			return DHCPOptions{}, r.ErrUnavailable
		}
		start := uintptr(unsafe.Pointer(&buffer[0]))
		pointer := uintptr(unsafe.Pointer(parameter.Data))
		if parameter.Data == nil || pointer < start || pointer-start > uintptr(len(buffer)) || uintptr(parameter.Size) > uintptr(len(buffer))-(pointer-start) {
			return DHCPOptions{}, r.ErrInvalidResponse
		}
		result := slices.Clone(unsafe.Slice(parameter.Data, parameter.Size))
		runtime.KeepAlive(buffer)
		return DHCPOptions{V4: result}, nil
	}
	return DHCPOptions{}, r.ErrCapacity
}
