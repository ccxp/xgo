package xmq

// #include <unistd.h>
//
import "C"

import (
	"net"
	"sync/atomic"
	"time"

	"github.com/ccxp/xgo/xnet"
)

func getUUID() (uint64, uint64) {
	return uuidX, atomic.AddUint64(&uuidY, 1)
}

var localip uint32
var pid uint32
var uuidX, uuidY uint64

func getLocalIp() uint32 {

	var uin, eth0, ret uint32
	ip := ""
	var lo1 = xnet.Htonl(xnet.Inet_aton("127.0.0.0"))
	var lo2 = xnet.Htonl(xnet.Inet_aton("127.255.255.255"))

	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			ip = ""

			switch v := addr.(type) {
			case *net.IPNet:
				if v.IP.To4() != nil {
					ip = v.IP.String()
				}
			case *net.IPAddr:

				if v.IP.To4() != nil {
					ip = v.IP.String()
				}
			}

			if ip == "" {
				continue
			}
			uin = xnet.Htonl(xnet.Inet_aton(ip))
			if lo1 <= uin && uin <= lo2 {
				continue
			}
			ret = uin
			if iface.Name == "eth1" {
				return ret
			}
			if iface.Name == "eth0" {
				eth0 = ret
			}
		}
	}
	if eth0 != 0 {
		return eth0
	}
	return ret
}

func init() {
	localip = getLocalIp()
	pid = uint32(C.getpid())
	uuidX = uint64(localip)<<32 | uint64(pid)
	uuidY = uint64(time.Now().UnixNano())
}
