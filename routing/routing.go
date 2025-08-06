package routing

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"syscall"
	"unsafe"
)

type Router interface {
	Route(dst net.IP) (iface *net.Interface, gateway, preferredSrc net.IP, err error)
	RouteWithSrc(input net.HardwareAddr, src, dst net.IP) (iface *net.Interface, gateway, preferredSrc net.IP, err error)
}

// http://man7.org/linux/man-pages/man7/rtnetlink.7.html
// 'struct rtmsg'
type routeInfoInMemory struct {
	Family byte
	DstLen byte
	SrcLen byte
	TOS    byte

	Table    byte
	Protocol byte
	Scope    byte
	Type     byte

	Flags uint32
}

// rtInfo contains information on a single route.
type rtInfo struct {
	Src, Dst         *net.IPNet
	Gateway, PrefSrc net.IP
	// We currently ignore the InputIface.
	InputIface, OutputIface uint32
	Priority                uint32
}

// routeSlice implements sort.Interface to sort routes by Priority.
type routeSlice []*rtInfo

func (r routeSlice) Len() int {
	return len(r)
}
func (r routeSlice) Less(i, j int) bool {
	return r[i].Priority < r[j].Priority
}
func (r routeSlice) Swap(i, j int) {
	r[i], r[j] = r[j], r[i]
}

type router struct {
	ifaces map[int]*net.Interface
	addrs  map[int]ipAddrs
	v4     routeSlice
}

func (r *router) String() string {
	strs := []string{"ROUTER", "--- V4 ---"}
	for _, route := range r.v4 {
		strs = append(strs, fmt.Sprintf("%+v", *route))
	}
	return strings.Join(strs, "\n")
}

type ipAddrs struct {
	v4 net.IP
}

func (r *router) Route(dst net.IP) (iface *net.Interface, gateway, preferredSrc net.IP, err error) {
	return r.RouteWithSrc(nil, nil, dst)
}

func (r *router) RouteWithSrc(input net.HardwareAddr, src, dst net.IP) (iface *net.Interface, gateway, preferredSrc net.IP, err error) {
	var ifaceIndex int
	switch {
	case dst.To4() != nil:
		ifaceIndex, gateway, preferredSrc, err = r.route(r.v4, input, src, dst)
	default:
		err = errors.New("only IPv4 supported")
	}

	if err != nil {
		return
	}

	iface = r.ifaces[ifaceIndex]

	if preferredSrc == nil {
		preferredSrc = r.addrs[ifaceIndex].v4
	}
	return
}

func (r *router) route(routes routeSlice, input net.HardwareAddr, src, dst net.IP) (iface int, gateway, preferredSrc net.IP, err error) {
	var inputIndex uint32
	if input != nil {
		for i, iface := range r.ifaces {
			if bytes.Equal(input, iface.HardwareAddr) {
				inputIndex = uint32(i)
				break
			}
		}
	}
	var defaultGateway *rtInfo = nil
	for _, rt := range routes {
		if rt.InputIface != 0 && rt.InputIface != inputIndex {
			continue
		}
		if rt.Src == nil && rt.Dst == nil {
			defaultGateway = rt
			continue
		}
		if rt.Src != nil && !rt.Src.Contains(src) {
			continue
		}
		if rt.Dst != nil && !rt.Dst.Contains(dst) {
			continue
		}
		return int(rt.OutputIface), rt.Gateway, rt.PrefSrc, nil
	}

	if defaultGateway != nil {
		return int(defaultGateway.OutputIface), defaultGateway.Gateway, defaultGateway.PrefSrc, nil
	}
	err = fmt.Errorf("route not found: %v", dst)
	return
}

func New() (Router, error) {
	rtr := &router{
		ifaces: make(map[int]*net.Interface),
		addrs:  make(map[int]ipAddrs),
	}
	tab, err := syscall.NetlinkRIB(syscall.RTM_GETROUTE, syscall.AF_INET)
	if err != nil {
		return nil, err
	}
	msgs, err := syscall.ParseNetlinkMessage(tab)
	if err != nil {
		return nil, err
	}
loop:
	for _, m := range msgs {
		switch m.Header.Type {
		case syscall.NLMSG_DONE:
			break loop
		case syscall.RTM_NEWROUTE:
			rt := (*routeInfoInMemory)(unsafe.Pointer(&m.Data[0]))
			routeInfo := rtInfo{}
			attrs, err := syscall.ParseNetlinkRouteAttr(&m)
			if err != nil {
				return nil, err
			}
			switch rt.Family {
			case syscall.AF_INET:
				rtr.v4 = append(rtr.v4, &routeInfo)
			default:
				continue loop
			}
			for _, attr := range attrs {
				switch attr.Attr.Type {
				case syscall.RTA_DST:
					routeInfo.Dst = &net.IPNet{
						IP:   attr.Value,
						Mask: net.CIDRMask(int(rt.DstLen), len(attr.Value)*8),
					}
				case syscall.RTA_SRC:
					routeInfo.Src = &net.IPNet{
						IP:   attr.Value,
						Mask: net.CIDRMask(int(rt.SrcLen), len(attr.Value)*8),
					}
				case syscall.RTA_GATEWAY:
					routeInfo.Gateway = attr.Value
				case syscall.RTA_PREFSRC:
					routeInfo.PrefSrc = attr.Value
				case syscall.RTA_IIF:
					routeInfo.InputIface = *(*uint32)(unsafe.Pointer(&attr.Value[0]))
				case syscall.RTA_OIF:
					routeInfo.OutputIface = *(*uint32)(unsafe.Pointer(&attr.Value[0]))
				case syscall.RTA_PRIORITY:
					routeInfo.Priority = *(*uint32)(unsafe.Pointer(&attr.Value[0]))
				}
			}
		}
	}
	sort.Sort(rtr.v4)
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, tmp := range ifaces {
		iface := tmp
		rtr.ifaces[iface.Index] = &iface
		var addrs ipAddrs
		ifaceAddrs, err := iface.Addrs()
		if err != nil {
			return nil, err
		}
		for _, addr := range ifaceAddrs {
			if inet, ok := addr.(*net.IPNet); ok {
				if v4 := inet.IP.To4(); v4 != nil {
					if addrs.v4 == nil {
						addrs.v4 = v4
						break
					}
				}
			}
		}
		rtr.addrs[iface.Index] = addrs
	}
	return rtr, nil
}
