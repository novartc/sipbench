package arp

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
)

var (
	table map[string][6]uint8
	mu    sync.RWMutex
)

func MustInit() {
	var err error
	table, err = readTable()
	if err != nil {
		panic(fmt.Sprintf("unable to read arp table: %v", err))
	}
}

func readTable() (map[string][6]uint8, error) {
	f, err := os.Open("/proc/net/arp")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	t := make(map[string][6]uint8, 8)
	s := bufio.NewScanner(f)
	s.Scan()
	for s.Scan() {
		fields := strings.Fields(s.Text())
		mac, err := net.ParseMAC(fields[3])
		if err != nil {
			return nil, fmt.Errorf("unable to parse MAC %s: %s", fields[0], fields[3])
		}
		t[fields[0]] = [6]uint8(mac)
	}

	return t, nil
}

func Get(addr string) ([6]uint8, bool) {
	mu.RLock()
	mac, ok := table[addr]
	mu.RUnlock()
	return mac, ok
}

func Reload() error {
	newTable, err := readTable()
	if err != nil {
		return err
	}
	mu.Lock()
	table = newTable
	mu.Unlock()
	return nil
}
