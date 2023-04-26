package xnet

import (
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"
	"unsafe"
)

// 取某个网卡的ip
// name=""为任意取一个ip
func GetEthIp(name string) string {
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		if name != "" && i.Name != name {
			continue
		}

		addrs, _ := i.Addrs()
		for _, addr := range addrs {
			switch v := addr.(type) {
			case *net.IPNet:
				if name == "" && v.IP.String() == "127.0.0.1" {
					continue
				}

				if v.IP.To4() != nil {
					return v.IP.String()
				}
			case *net.IPAddr:
				if name == "" && v.IP.String() == "127.0.0.1" {
					continue
				}

				if v.IP.To4() != nil {
					return v.IP.String()
				}
			}
		}
	}
	if name == "" {
		return "127.0.0.1"
	}
	return ""
}

func Htons(i uint16) uint16 {
	if binary_order_is_big_endian {
		return i
	}

	return i<<8 | i>>8
}

func Htonl(i uint32) uint32 {
	if binary_order_is_big_endian {
		return i
	}

	return i<<24 | (i&0xff00)<<8 | (i&0xff0000)>>8 | i>>24
}

func Ntohs(i uint16) uint16 {
	if binary_order_is_big_endian {
		return i
	}

	return i<<8 | i>>8
}

func Ntohl(i uint32) uint32 {
	if binary_order_is_big_endian {
		return i
	}

	return i<<24 | (i&0xff00)<<8 | (i&0xff0000)>>8 | i>>24
}

func Inet_aton(ip string) uint32 {
	bits := strings.Split(ip, ".")
	if len(bits) != 4 {
		return 0
	}

	b0, e1 := strconv.ParseUint(bits[0], 10, 8)
	b1, e2 := strconv.ParseUint(bits[1], 10, 8)
	b2, e3 := strconv.ParseUint(bits[2], 10, 8)
	b3, e4 := strconv.ParseUint(bits[3], 10, 8)

	if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
		return 0
	}
	if b0 > 255 || b1 > 255 || b2 > 255 || b3 > 255 {
		return 0
	}

	return uint32(b3<<24 | b2<<16 | b1<<8 | b0)
}

func Inet_ntoa(ip uint32) string {
	b0 := ip & 0xff
	b1 := (ip >> 8) & 0xff
	b2 := (ip >> 16) & 0xff
	b3 := ip >> 24
	return fmt.Sprintf("%d.%d.%d.%d", b0, b1, b2, b3)
}

var binary_order_is_big_endian bool = false

func init() {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, 1)
	v := *(*uint32)(unsafe.Pointer(&b[0]))

	if v == 1 {
		binary_order_is_big_endian = true
	}
}

// Hash consistently chooses a hash bucket number in the range [0, numBuckets) for the given key. numBuckets must be >= 1.
func JumpHash(key uint64, numBuckets int) int {

	var b int64 = -1
	var j int64

	for j < int64(numBuckets) {
		b = j
		key = key*2862933555777941757 + 1
		j = int64(float64(b+1) * (float64(int64(1)<<31) / float64((key>>33)+1)))
	}

	return int(b)
}
