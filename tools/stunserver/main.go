package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"net"
)

const (
	magicCookie    uint32 = 0x2112A442
	bindingRequest uint16 = 0x0001
	bindingSuccess uint16 = 0x0101
	attrXorMapped  uint16 = 0x0020
	attrMapped     uint16 = 0x0001
	attrSoftware   uint16 = 0x8022
	headerSize            = 20
)

var softwareValue = []byte("stun-max")

func main() {
	addr := flag.String("addr", ":3478", "STUN listen address (UDP)")
	flag.Parse()

	pc, err := net.ListenPacket("udp", *addr)
	if err != nil {
		log.Fatalf("Listen failed: %v", err)
	}
	defer pc.Close()

	fmt.Println("═══════════════════════════════════════")
	fmt.Println("  STUN Max - STUN Server")
	fmt.Println("═══════════════════════════════════════")
	fmt.Printf("  Listening on UDP %s\n", *addr)
	fmt.Println("═══════════════════════════════════════")

	buf := make([]byte, 1500)
	for {
		n, raddr, err := pc.ReadFrom(buf)
		if err != nil {
			log.Printf("Read error: %v", err)
			continue
		}
		if n < headerSize {
			continue
		}

		msgType := binary.BigEndian.Uint16(buf[0:2])
		if msgType != bindingRequest {
			continue
		}

		txID := make([]byte, 12)
		copy(txID, buf[8:20])

		udpAddr := raddr.(*net.UDPAddr)
		resp := buildBindingResponse(txID, udpAddr)

		if _, err := pc.WriteTo(resp, raddr); err != nil {
			log.Printf("Write error to %s: %v", raddr, err)
		}
	}
}

func buildBindingResponse(txID []byte, addr *net.UDPAddr) []byte {
	ip4 := addr.IP.To4()
	if ip4 == nil {
		return nil
	}

	// XOR-MAPPED-ADDRESS attribute (8 bytes value)
	xorMapped := make([]byte, 12) // 4 header + 8 value
	binary.BigEndian.PutUint16(xorMapped[0:2], attrXorMapped)
	binary.BigEndian.PutUint16(xorMapped[2:4], 8)
	xorMapped[4] = 0x00 // reserved
	xorMapped[5] = 0x01 // IPv4
	binary.BigEndian.PutUint16(xorMapped[6:8], uint16(addr.Port)^uint16(magicCookie>>16))
	ipInt := binary.BigEndian.Uint32(ip4)
	binary.BigEndian.PutUint32(xorMapped[8:12], ipInt^magicCookie)

	// MAPPED-ADDRESS attribute (for older clients)
	mapped := make([]byte, 12)
	binary.BigEndian.PutUint16(mapped[0:2], attrMapped)
	binary.BigEndian.PutUint16(mapped[2:4], 8)
	mapped[4] = 0x00
	mapped[5] = 0x01
	binary.BigEndian.PutUint16(mapped[6:8], uint16(addr.Port))
	binary.BigEndian.PutUint32(mapped[8:12], ipInt)

	// SOFTWARE attribute
	swPad := len(softwareValue)
	if swPad%4 != 0 {
		swPad += 4 - (swPad % 4)
	}
	software := make([]byte, 4+swPad)
	binary.BigEndian.PutUint16(software[0:2], attrSoftware)
	binary.BigEndian.PutUint16(software[2:4], uint16(len(softwareValue)))
	copy(software[4:], softwareValue)

	// Total attributes length
	attrsLen := len(xorMapped) + len(mapped) + len(software)

	// Build response
	resp := make([]byte, headerSize+attrsLen)
	binary.BigEndian.PutUint16(resp[0:2], bindingSuccess)
	binary.BigEndian.PutUint16(resp[2:4], uint16(attrsLen))
	binary.BigEndian.PutUint32(resp[4:8], magicCookie)
	copy(resp[8:20], txID)

	offset := headerSize
	copy(resp[offset:], xorMapped)
	offset += len(xorMapped)
	copy(resp[offset:], mapped)
	offset += len(mapped)
	copy(resp[offset:], software)

	return resp
}
