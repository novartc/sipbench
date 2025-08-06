package routing

import (
	"fmt"
	"net"
)

var (
	r Router
)

func MustInit() {
	var err error
	r, err = New()
	if err != nil {
		panic(fmt.Sprintf("unable to read routing table: %v", err))
	}
}

func Reload() error {
	newRouter, err := New()
	if err != nil {
		return err
	}
	r = newRouter
	return nil
}

func Route(dst net.IP) (iface *net.Interface, gateway, preferredSrc net.IP, err error) {
	return r.Route(dst)
}
