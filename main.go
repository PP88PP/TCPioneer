package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/chai2010/winsvc"
	"github.com/williamfhe/godivert"
)

type Config struct {
	Level    uint16
	ANCount4 uint16
	ANCount6 uint16
	Answers4 []byte
	Answers6 []byte
}

var DomainMap map[string]Config
var IPMap map[string]int
var DNS string
var TTL int = 0
var MSS int = 1024
var LocalDNS bool = false
var ServiceMode bool = true
var IPv6Enable = false
var LogLevel = 0

func TCPlookup(request []byte, address string) ([]byte, error) {
	server, err := net.Dial("tcp", address)
	if err != nil {
		return nil, err
	}
	defer server.Close()
	data := make([]byte, 4096)
	binary.BigEndian.PutUint16(data[:2], uint16(len(request)))
	copy(data[2:], request)

	_, err = server.Write(data[:len(request)+2])
	if err != nil {
		return nil, err
	}

	length := 0
	recvlen := 0
	for {
		n, err := server.Read(data[length:])
		if err != nil {
			return nil, err
		}
		if length == 0 {
			length = int(binary.BigEndian.Uint16(data[:2]) + 2)
		}
		recvlen += n
		if recvlen >= length {
			return data[2:recvlen], nil
		}
	}

	return nil, nil
}

func getQName(buf []byte) (string, int, int) {
	bufflen := len(buf)
	if bufflen < 13 {
		logPrintln("sf1")
		return "", 0, 0
	}
	length := buf[12]
	off := 13
	end := off + int(length)
	qname := string(buf[off:end])
	off = end

	for {
		if off >= bufflen {
			return "", 0, 0
		}
		length := buf[off]
		off++
		if length == 0x00 {
			break
		}
		end := off + int(length)
		if end > bufflen {
			return "", 0, 0
		}
		qname += "." + string(buf[off:end])
		off = end
	}
	end = off + 4
	if end > bufflen {
		return "", 0, 0
	}

	qtype := int(binary.BigEndian.Uint16(buf[off : off+2]))

	return qname, qtype, end
}

func domainLookup(qname string) Config {
	config, ok := DomainMap[qname]
	if ok {
		return config
	}

	offset := 0
	for i := 0; i < 2; i++ {
		off := strings.Index(qname[offset:], ".")
		if off == -1 {
			return Config{0, 0, 0, nil, nil}
		}
		offset += off
		config, ok = DomainMap[qname[offset:]]
		if ok {
			return config
		}
		offset++
	}

	return Config{0, 0, 0, nil, nil}
}

func getAnswers(answers []byte, count int) []string {
	ips := make([]string, 0)
	offset := 0

	for i := 0; i < count; i++ {
		for {
			if offset >= len(answers) {
				return nil
			}
			length := answers[offset]
			offset++
			if length == 0 {
				break
			}
			if length < 63 {
				offset += int(length)
				if offset+2 > len(answers) {
					return nil
				}
			} else {
				offset++
				break
			}
		}
		if offset+2 > len(answers) {
			return nil
		}
		AType := binary.BigEndian.Uint16(answers[offset : offset+2])
		offset += 8
		if offset+2 > len(answers) {
			return nil
		}
		DataLength := binary.BigEndian.Uint16(answers[offset : offset+2])
		offset += 2

		if AType == 1 {
			if offset+4 > len(answers) {
				return nil
			}
			data := answers[offset : offset+4]
			ip := net.IPv4(data[0], data[1], data[2], data[3]).String()
			ips = append(ips, ip)
		} else if AType == 28 {
			var data [16]byte
			if offset+16 > len(answers) {
				return nil
			}
			copy(data[:], answers[offset:offset+16])
			ip := net.IP(answers[offset : offset+16]).String()
			ips = append(ips, ip)
		}

		offset += int(DataLength)
	}

	return ips
}

func packAnswers(ips []string, qtype int) (int, []byte) {
	totalLen := 0
	count := 0
	for _, ip := range ips {
		ip4 := net.ParseIP(ip).To4()
		if ip4 != nil && qtype == 1 {
			count++
			totalLen += 16
		} else if qtype == 28 {
			count++
			totalLen += 28
		}
	}

	answers := make([]byte, totalLen)
	length := 0
	for _, strIP := range ips {
		ip := net.ParseIP(strIP)
		ip4 := ip.To4()
		if ip4 != nil {
			if qtype == 1 {
				answer := []byte{0xC0, 0x0C, 0x00, 1,
					0x00, 0x01, 0x00, 0x00, 0x0E, 0x10, 0x00, 0x04,
					ip4[0], ip4[1], ip4[2], ip4[3]}
				copy(answers[length:], answer)
				length += 16
			}
		} else {
			if qtype == 28 {
				answer := []byte{0xC0, 0x0C, 0x00, 28,
					0x00, 0x01, 0x00, 0x00, 0x0E, 0x10, 0x00, 0x10}
				copy(answers[length:], answer)
				length += 12
				copy(answers[length:], ip)
				length += 16
			}
		}
	}

	return count, answers
}

func DNSDaemon() {
	arg := []string{"/flushdns"}
	cmd := exec.Command("ipconfig", arg...)
	d, err := cmd.CombinedOutput()
	if err != nil {
		if LogLevel > 0 || !ServiceMode {
			log.Println(string(d), err)
		}
		return
	}

	filter := "outbound and udp.DstPort == 53"
	winDivert, err := godivert.NewWinDivertHandle(filter)
	if err != nil {
		if LogLevel > 0 || !ServiceMode {
			log.Println(err, filter)
		}
		return
	}
	defer winDivert.Close()

	rawbuf := make([]byte, 1500)
	for {
		packet, err := winDivert.Recv()
		if err != nil {
			if LogLevel > 0 || !ServiceMode {
				log.Println(err)
			}
			continue
		}

		ipv6 := packet.Raw[0]>>4 == 6

		var ipheadlen int
		if ipv6 {
			ipheadlen = 40
		} else {
			ipheadlen = int(packet.Raw[0]&0xF) * 4
		}
		udpheadlen := 8
		qname, qtype, off := getQName(packet.Raw[ipheadlen+udpheadlen:])
		if qname == "" {
			logPrintln("DNS Segmentation fault")
			continue
		}

		config := domainLookup(qname)
		if config.Level > 0 {
			var anCount uint16 = 0
			var answers []byte = nil

			var noRecord bool
			if !IPv6Enable && qtype == 28 {
				noRecord = true
			} else {
				if qtype == 1 {
					if config.ANCount6 > 0 && config.ANCount4 == 0 {
						noRecord = true
					} else {
						answers = config.Answers4
						anCount = config.ANCount4
					}
				} else if qtype == 28 {
					if config.ANCount4 > 0 && config.ANCount6 == 0 {
						noRecord = true
					} else {
						answers = config.Answers6
						anCount = config.ANCount6
					}
				}
			}

			if noRecord {
				request := packet.Raw[ipheadlen+udpheadlen:]
				udpsize := len(request) + 8

				var packetsize int
				if ipv6 {
					copy(rawbuf, []byte{96, 12, 19, 68, 0, 98, 17, 128})
					packetsize = 40 + udpsize
					binary.BigEndian.PutUint16(rawbuf[4:], uint16(udpsize))
					copy(rawbuf[8:], packet.Raw[24:40])
					copy(rawbuf[24:], packet.Raw[8:24])
				} else {
					copy(rawbuf, []byte{69, 0, 1, 32, 141, 152, 64, 0, 64, 17, 150, 46})
					packetsize = 20 + udpsize
					binary.BigEndian.PutUint16(rawbuf[2:], uint16(packetsize))
					copy(rawbuf[12:], packet.Raw[16:20])
					copy(rawbuf[16:], packet.Raw[12:16])
					ipheadlen = 20
				}

				copy(rawbuf[ipheadlen:], packet.Raw[ipheadlen+2:ipheadlen+4])
				copy(rawbuf[ipheadlen+2:], packet.Raw[ipheadlen:ipheadlen+2])
				binary.BigEndian.PutUint16(rawbuf[ipheadlen+4:], uint16(udpsize))
				copy(rawbuf[ipheadlen+8:], request)
				rawbuf[ipheadlen+10] = 0x81
				rawbuf[ipheadlen+11] = 0x80
				binary.BigEndian.PutUint16(rawbuf[ipheadlen+14:], 0)

				packet.PacketLen = uint(packetsize)
				packet.Raw = rawbuf[:packetsize]
				packet.CalcNewChecksum(winDivert)

				_, err = winDivert.Send(packet)
			} else {
				if anCount > 0 {
					logPrintln(qname)
					request := packet.Raw[ipheadlen+udpheadlen:]
					udpsize := len(request) + len(answers) + 8

					var packetsize int
					if ipv6 {
						copy(rawbuf, []byte{96, 12, 19, 68, 0, 98, 17, 128})
						packetsize = 40 + udpsize
						binary.BigEndian.PutUint16(rawbuf[4:], uint16(udpsize))
						copy(rawbuf[8:], packet.Raw[24:40])
						copy(rawbuf[24:], packet.Raw[8:24])
					} else {
						copy(rawbuf, []byte{69, 0, 1, 32, 141, 152, 64, 0, 64, 17, 150, 46})
						packetsize = 20 + udpsize
						binary.BigEndian.PutUint16(rawbuf[2:], uint16(packetsize))
						copy(rawbuf[12:], packet.Raw[16:20])
						copy(rawbuf[16:], packet.Raw[12:16])
						ipheadlen = 20
					}

					copy(rawbuf[ipheadlen:], packet.Raw[ipheadlen+2:ipheadlen+4])
					copy(rawbuf[ipheadlen+2:], packet.Raw[ipheadlen:ipheadlen+2])
					binary.BigEndian.PutUint16(rawbuf[ipheadlen+4:], uint16(udpsize))
					copy(rawbuf[ipheadlen+8:], request)
					rawbuf[ipheadlen+10] = 0x81
					rawbuf[ipheadlen+11] = 0x80
					binary.BigEndian.PutUint16(rawbuf[ipheadlen+14:], anCount)
					copy(rawbuf[ipheadlen+8+len(request):], answers)

					packet.PacketLen = uint(packetsize)
					packet.Raw = rawbuf[:packetsize]
					packet.CalcNewChecksum(winDivert)

					_, err = winDivert.Send(packet)
				} else if !LocalDNS {
					logPrintln(qname, config.Level)
					go func(level int) {
						response, err := TCPlookup(packet.Raw[ipheadlen+udpheadlen:], DNS)
						if err != nil {
							if LogLevel > 1 || !ServiceMode {
								log.Println(err)
							}
							return
						}

						count := int(binary.BigEndian.Uint16(response[6:8]))
						ips := getAnswers(response[off:], count)
						for _, ip := range ips {
							IPMap[ip] = int(level)
						}

						rawbuf := make([]byte, 1500)
						copy(rawbuf, []byte{69, 0, 1, 32, 141, 152, 64, 0, 64, 17, 150, 46})
						packetsize := 28 + len(response)
						binary.BigEndian.PutUint16(rawbuf[2:], uint16(packetsize))
						copy(rawbuf[12:], packet.Raw[16:20])
						copy(rawbuf[16:], packet.Raw[12:16])
						copy(rawbuf[20:], packet.Raw[22:24])
						copy(rawbuf[22:], packet.Raw[20:22])
						binary.BigEndian.PutUint16(rawbuf[24:], uint16(len(response)+8))
						copy(rawbuf[28:], response)

						packet.PacketLen = uint(packetsize)
						packet.Raw = rawbuf[:packetsize]
						packet.CalcNewChecksum(winDivert)

						_, err = winDivert.Send(packet)
					}(int(config.Level))
				}
			}
		} else {
			_, err = winDivert.Send(packet)
		}
	}
}

func DNSRecvDaemon() {
	filter := "((outbound and loopback) or inbound) and udp.SrcPort == 53"
	winDivert, err := godivert.NewWinDivertHandle(filter)
	if err != nil {
		if LogLevel > 0 || !ServiceMode {
			log.Println(err, filter)
		}
		return
	}
	defer winDivert.Close()

	for {
		packet, err := winDivert.Recv()
		if err != nil {
			if LogLevel > 1 || !ServiceMode {
				log.Println(err)
			}
			return
		}

		ipv6 := packet.Raw[0]>>4 == 6

		var ipheadlen int
		if ipv6 {
			ipheadlen = 40
		} else {
			ipheadlen = int(packet.Raw[0]&0xF) * 4
		}

		udpheadlen := 8
		qname, qtype, off := getQName(packet.Raw[ipheadlen+udpheadlen:])
		if qname == "" {
			logPrintln("DNS Segmentation fault")
			continue
		}

		config := domainLookup(qname)

		if config.Level > 1 && qtype == 1 {
			logPrintln(qname, "LEVEL", config.Level)
			response := packet.Raw[ipheadlen+udpheadlen:]
			count := int(binary.BigEndian.Uint16(response[6:8]))
			ips := getAnswers(response[off:], count)

			for _, ip := range ips {
				IPMap[ip] = int(config.Level)
			}
		}
		_, err = winDivert.Send(packet)
	}
}

func DOTDaemon() {
	filter := "tcp.Psh and tcp.DstPort == 53"
	winDivert, err := godivert.NewWinDivertHandle(filter)
	if err != nil {
		if LogLevel > 0 || !ServiceMode {
			log.Println(err, filter)
		}
		return
	}
	defer winDivert.Close()

	rawbuf := make([]byte, 1500)
	prefix_rawbuf := make([]byte, 1500)

	for {
		packet, err := winDivert.Recv()
		if err != nil {
			if LogLevel > 0 || !ServiceMode {
				log.Println(err)
			}
			return
		}

		ipv6 := packet.Raw[0]>>4 == 6

		var ipheadlen int
		if ipv6 {
			ipheadlen = 40
		} else {
			ipheadlen = int(packet.Raw[0]&0xF) * 4
		}
		tcpheadlen := int(packet.Raw[ipheadlen+12]>>4) * 4

		fake_packet := *packet
		if TTL > 0 {
			copy(rawbuf, packet.Raw[:ipheadlen+tcpheadlen])
			if ipv6 {
				rawbuf[7] = byte(TTL)
			} else {
				rawbuf[8] = byte(TTL)
			}
		} else {
			copy(rawbuf, packet.Raw[:ipheadlen+20])
			copy(rawbuf[ipheadlen+20:], []byte{19, 18, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
			rawbuf[ipheadlen+12] = 10 << 4
		}
		fake_packet.Raw = rawbuf[:len(packet.Raw)]
		fake_packet.CalcNewChecksum(winDivert)

		_, err = winDivert.Send(&fake_packet)
		if err != nil {
			if LogLevel > 0 || !ServiceMode {
				log.Println(err)
			}
			return
		}

		cut_offset := 20
		total_cut_offset := ipheadlen + tcpheadlen + cut_offset

		copy(prefix_rawbuf, packet.Raw[:total_cut_offset])
		binary.BigEndian.PutUint16(prefix_rawbuf[2:], uint16(total_cut_offset))
		prefix_packet := *packet
		prefix_packet.Raw = prefix_rawbuf[:total_cut_offset]
		prefix_packet.PacketLen = uint(total_cut_offset)
		prefix_packet.CalcNewChecksum(winDivert)
		_, err = winDivert.Send(&prefix_packet)
		if err != nil {
			if LogLevel > 0 || !ServiceMode {
				log.Println(err)
			}
			return
		}

		_, err = winDivert.Send(&fake_packet)
		if err != nil {
			if LogLevel > 0 || !ServiceMode {
				log.Println(err)
			}
			return
		}

		seqNum := binary.BigEndian.Uint32(packet.Raw[ipheadlen+4 : ipheadlen+8])
		copy(rawbuf, packet.Raw[:ipheadlen+tcpheadlen])
		copy(rawbuf[ipheadlen+tcpheadlen:], packet.Raw[total_cut_offset:])
		totallen := uint16(packet.PacketLen) - uint16(cut_offset)
		binary.BigEndian.PutUint16(rawbuf[2:], totallen)
		binary.BigEndian.PutUint32(rawbuf[ipheadlen+4:], seqNum+uint32(cut_offset))
		packet.Raw = rawbuf[:totallen]
		packet.PacketLen = uint(totallen)
		packet.CalcNewChecksum(winDivert)

		_, err = winDivert.Send(packet)
		if err != nil {
			if LogLevel > 0 || !ServiceMode {
				log.Println(err)
			}
			return
		}
	}
}

func HTTPDaemon() {
	filter := "tcp.Psh and tcp.DstPort == 80"
	winDivert, err := godivert.NewWinDivertHandle(filter)
	if err != nil {
		if LogLevel > 0 || !ServiceMode {
			log.Println(err, filter)
		}
		return
	}
	defer winDivert.Close()

	rawbuf := make([]byte, 1500)

	for {
		packet, err := winDivert.Recv()
		if err != nil {
			if LogLevel > 0 || !ServiceMode {
				log.Println(err)
			}
			return
		}

		level, ok := IPMap[packet.DstIP().String()]

		if ok {
			if level > 1 {
				ipv6 := packet.Raw[0]>>4 == 6

				var ipheadlen int
				if ipv6 {
					ipheadlen = 40
				} else {
					ipheadlen = int(packet.Raw[0]&0xF) * 4
				}
				tcpheadlen := int(packet.Raw[ipheadlen+12]>>4) * 4

				fake_packet := *packet
				if TTL > 0 {
					copy(rawbuf, packet.Raw[:ipheadlen+tcpheadlen])
					rawbuf[8] = byte(TTL)
				} else {
					copy(rawbuf, packet.Raw[:ipheadlen+20])
					copy(rawbuf[ipheadlen+20:], []byte{19, 18, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
					rawbuf[ipheadlen+12] = 10 << 4
				}
				fake_packet.Raw = rawbuf[:len(packet.Raw)]
				fake_packet.CalcNewChecksum(winDivert)

				_, err = winDivert.Send(&fake_packet)
				if err != nil {
					if LogLevel > 0 || !ServiceMode {
						log.Println(err)
					}
					return
				}

				_, err = winDivert.Send(&fake_packet)
				if err != nil {
					if LogLevel > 0 || !ServiceMode {
						log.Println(err)
					}
					return
				}
			}
		}

		time.Sleep(time.Microsecond * 10)

		_, err = winDivert.Send(packet)
		if err != nil {
			if LogLevel > 0 || !ServiceMode {
				log.Println(err)
			}
			return
		}
	}
}

func getSNI(b []byte) (offset int, length int) {
	Length := binary.BigEndian.Uint16(b[3:5])
	if len(b) <= int(Length)-5 {
		return 0, 0
	}
	offset = 11 + 32
	SessionIDLength := b[offset]
	offset += 1 + int(SessionIDLength)
	CipherSuitersLength := binary.BigEndian.Uint16(b[offset : offset+2])
	offset += 2 + int(CipherSuitersLength)
	if offset >= len(b) {
		return 0, 0
	}
	CompressionMethodsLenght := b[offset]
	offset += 1 + int(CompressionMethodsLenght)
	ExtensionsLength := binary.BigEndian.Uint16(b[offset : offset+2])
	offset += 2
	ExtensionsEnd := offset + int(ExtensionsLength)
	for offset < ExtensionsEnd {
		ExtensionType := binary.BigEndian.Uint16(b[offset : offset+2])
		offset += 2
		ExtensionLength := binary.BigEndian.Uint16(b[offset : offset+2])
		offset += 2
		if ExtensionType == 0 {
			offset += 2
			offset++
			ServerNameLength := binary.BigEndian.Uint16(b[offset : offset+2])
			offset += 2
			return offset, int(ServerNameLength)
		} else {
			offset += int(ExtensionLength)
		}
	}
	return 0, 0
}

func hello(SrcPort int, TTL int) {
	filter := "tcp.Psh and tcp.SrcPort == " + strconv.Itoa(SrcPort)
	winDivert, err := godivert.NewWinDivertHandle(filter)
	if err != nil {
		if LogLevel > 0 || !ServiceMode {
			log.Println(err, filter)
		}
		return
	}
	defer winDivert.Close()

	ch := make(chan *godivert.Packet)
	go func() {
		packet, err := winDivert.Recv()
		if err != nil {
			return
		}

		ch <- packet
	}()

	var packet *godivert.Packet
	select {
	case res := <-ch:
		packet = res
	case <-time.After(time.Second * 32):
		return
	}

	ipv6 := packet.Raw[0]>>4 == 6

	var ipheadlen int
	if ipv6 {
		ipheadlen = 40
	} else {
		ipheadlen = int(packet.Raw[0]&0xF) * 4
	}
	tcpheadlen := int(packet.Raw[ipheadlen+12]>>4) * 4
	sni_offset, sni_length := getSNI(packet.Raw[ipheadlen+tcpheadlen:])

	rawbuf := make([]byte, 1500)
	if sni_length > 0 {
		fake_packet := *packet
		if TTL > 0 {
			copy(rawbuf, packet.Raw[:ipheadlen+tcpheadlen])
			if ipv6 {
				rawbuf[7] = byte(TTL)
			} else {
				rawbuf[8] = byte(TTL)
			}
		} else {
			copy(rawbuf, packet.Raw[:ipheadlen+20])
			copy(rawbuf[ipheadlen+20:], []byte{19, 18, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
			rawbuf[ipheadlen+12] = 10 << 4
		}
		fake_packet.Raw = rawbuf[:len(packet.Raw)]
		fake_packet.CalcNewChecksum(winDivert)

		_, err = winDivert.Send(&fake_packet)
		if err != nil {
			if LogLevel > 0 || !ServiceMode {
				log.Println(err)
			}
			return
		}

		sni_cut_offset := sni_offset + sni_length/2
		total_cut_offset := ipheadlen + tcpheadlen + sni_cut_offset

		prefix_rawbuf := make([]byte, 1500)
		copy(prefix_rawbuf, packet.Raw[:total_cut_offset])
		if ipv6 {
			binary.BigEndian.PutUint16(prefix_rawbuf[4:], uint16(total_cut_offset-ipheadlen))
		} else {
			binary.BigEndian.PutUint16(prefix_rawbuf[2:], uint16(total_cut_offset))
		}
		prefix_packet := *packet
		prefix_packet.Raw = prefix_rawbuf[:total_cut_offset]
		prefix_packet.PacketLen = uint(total_cut_offset)
		prefix_packet.CalcNewChecksum(winDivert)
		_, err = winDivert.Send(&prefix_packet)
		if err != nil {
			if LogLevel > 0 || !ServiceMode {
				log.Println(err)
			}
			return
		}

		_, err = winDivert.Send(&fake_packet)
		if err != nil {
			if LogLevel > 0 || !ServiceMode {
				log.Println(err)
			}
			return
		}

		seqNum := binary.BigEndian.Uint32(packet.Raw[ipheadlen+4 : ipheadlen+8])
		copy(rawbuf, packet.Raw[:ipheadlen+tcpheadlen])
		copy(rawbuf[ipheadlen+tcpheadlen:], packet.Raw[total_cut_offset:])
		totallen := uint16(packet.PacketLen) - uint16(sni_cut_offset)
		if ipv6 {
			binary.BigEndian.PutUint16(rawbuf[4:], totallen-uint16(ipheadlen))
		} else {
			binary.BigEndian.PutUint16(rawbuf[2:], totallen)
		}
		binary.BigEndian.PutUint32(rawbuf[ipheadlen+4:], seqNum+uint32(sni_cut_offset))
		packet.Raw = rawbuf[:totallen]
		packet.PacketLen = uint(totallen)
		packet.CalcNewChecksum(winDivert)
	}

	_, err = winDivert.Send(packet)
	if err != nil {
		if LogLevel > 0 || !ServiceMode {
			log.Println(err)
		}
		return
	}
}

func loadConfig() error {
	DomainMap = make(map[string]Config)
	IPMap = make(map[string]int)
	conf, err := os.Open("config")
	if err != nil {
		return err
	}
	defer conf.Close()

	br := bufio.NewReader(conf)
	level := 0
	for {
		line, _, err := br.ReadLine()
		if err == io.EOF {
			break
		}
		if len(line) > 0 {
			if line[0] == '#' {
				if string(line) == "#LEVEL0" {
					level = 0
				} else if string(line) == "#LEVEL1" {
					level = 1
				} else if string(line) == "#LEVEL2" {
					level = 2
				} else if string(line) == "#LEVEL3" {
					level = 3
				} else if string(line) == "#LEVEL4" {
					level = 4
				}
			} else {
				keys := strings.SplitN(string(line), "=", 2)
				if len(keys) > 1 {
					if keys[0] == "server" {
						DNS = keys[1]

						logPrintln(string(line))
						if DNS == "127.0.0.1:53" || DNS == "[::1]:53" {
							LocalDNS = true
							logPrintln("local-dns")
						}

					} else if keys[0] == "ttl" {
						TTL, err = strconv.Atoi(keys[1])
						if err != nil {
							log.Println(string(line), err)
							return err
						}
						logPrintln(string(line))
					} else if keys[0] == "mss" {
						MSS, err = strconv.Atoi(keys[1])
						if err != nil {
							log.Println(string(line), err)
							return err
						}
						logPrintln(string(line))
					} else if keys[0] == "log" {
						LogLevel, err = strconv.Atoi(keys[1])
						if err != nil {
							log.Println(string(line), err)
							return err
						}
					} else {
						ips := strings.Split(keys[1], ",")
						for _, ip := range ips {
							IPMap[ip] = level
						}
						count4, answer4 := packAnswers(ips, 1)
						count6, answer6 := packAnswers(ips, 28)
						DomainMap[keys[0]] = Config{uint16(level), uint16(count4), uint16(count6), answer4, answer6}
					}
				} else {
					if keys[0] == "local-dns" {
						LocalDNS = true
						logPrintln("local-dns")
					} else if keys[0] == "ipv6" {
						IPv6Enable = true
					} else {
						DomainMap[keys[0]] = Config{uint16(level), 0, 0, nil, nil}
					}
				}
			}
		}
	}

	return nil
}

var Logger *log.Logger

func logPrintln(v ...interface{}) {
	if LogLevel > 1 || !ServiceMode {
		log.Println(v)
	}
}

func StartService() {
	runtime.GOMAXPROCS(1)
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	if LogLevel > 0 {
		var logFilename string = "tcpioneer.log"
		logFile, err := os.OpenFile(logFilename, os.O_RDWR|os.O_CREATE, 0777)
		if err != nil {
			log.Println(err)
			return
		}
		defer logFile.Close()

		Logger = log.New(logFile, "\r\n", log.Ldate|log.Ltime|log.Lshortfile)
	}

	err := loadConfig()
	if err != nil {
		if LogLevel > 0 || !ServiceMode {
			log.Println(err)
		}
		return
	}

	filter := "outbound and !loopback and tcp.Syn == 1 and tcp.Ack == 0 and tcp.DstPort == 443"
	winDivert, err := godivert.NewWinDivertHandle(filter)
	if err != nil {
		if LogLevel > 0 || !ServiceMode {
			log.Println(err, filter)
		}
		return
	}
	defer winDivert.Close()

	go DNSDaemon()
	if LocalDNS {
		go DNSRecvDaemon()
	} else {
		go DOTDaemon()
	}

	go HTTPDaemon()

	for {
		packet, err := winDivert.Recv()
		if err != nil {
			if LogLevel > 0 || !ServiceMode {
				log.Println(err)
			}
			return
		}

		level, ok := IPMap[packet.DstIP().String()]

		if ok {
			ipv6 := packet.Raw[0]>>4 == 6

			var ipheadlen int
			if ipv6 {
				ipheadlen = 40
			} else {
				ipheadlen = int(packet.Raw[0]&0xF) * 4
			}

			if level > 2 {
				if len(packet.Raw) < ipheadlen+24 {
					logPrintln(packet)
					return
				}

				option := packet.Raw[ipheadlen+20]
				if option == 2 {
					binary.BigEndian.PutUint16(packet.Raw[ipheadlen+22:], uint16(MSS))
					packet.CalcNewChecksum(winDivert)
				}
			}

			if level > 1 {
				SrcPort := int(binary.BigEndian.Uint16(packet.Raw[ipheadlen:]))
				go hello(SrcPort, TTL)
				logPrintln(packet.DstIP(), "LEVEL", level)
			}
		}

		_, err = winDivert.Send(packet)
		if err != nil {
			if LogLevel > 0 || !ServiceMode {
				log.Println(err)
			}
			return
		}
	}
}

func StopService() {
	arg := []string{"/flushdns"}
	cmd := exec.Command("ipconfig", arg...)
	d, err := cmd.CombinedOutput()
	if err != nil {
		log.Println(string(d), err)
	}

	os.Exit(0)
}

func main() {
	serviceName := "TCPPioneer"
	var flagServiceInstall bool
	var flagServiceUninstall bool
	var flagServiceStart bool
	var flagServiceStop bool
	flag.BoolVar(&flagServiceInstall, "install", false, "Install service")
	flag.BoolVar(&flagServiceUninstall, "remove", false, "Remove service")
	flag.BoolVar(&flagServiceStart, "start", false, "Start service")
	flag.BoolVar(&flagServiceStop, "stop", false, "Stop service")
	flag.Parse()

	appPath, err := winsvc.GetAppPath()
	if err != nil {
		log.Fatal(err)
	}

	// install service
	if flagServiceInstall {
		if err := winsvc.InstallService(appPath, serviceName, ""); err != nil {
			log.Fatalf("installService(%s, %s): %v\n", serviceName, "", err)
		}
		log.Printf("Done\n")
		return
	}

	// remove service
	if flagServiceUninstall {
		if err := winsvc.RemoveService(serviceName); err != nil {
			log.Fatalln("removeService:", err)
		}
		log.Printf("Done\n")
		return
	}

	// start service
	if flagServiceStart {
		if err := winsvc.StartService(serviceName); err != nil {
			log.Fatalln("startService:", err)
		}
		log.Printf("Done\n")
		return
	}

	// stop service
	if flagServiceStop {
		if err := winsvc.StopService(serviceName); err != nil {
			log.Fatalln("stopService:", err)
		}
		log.Printf("Done\n")
		return
	}

	// run as service
	if !winsvc.IsAnInteractiveSession() {
		log.Println("main:", "runService")

		if err := os.Chdir(filepath.Dir(appPath)); err != nil {
			log.Fatal(err)
		}

		if err := winsvc.RunAsService(serviceName, StartService, StopService, false); err != nil {
			log.Fatalf("svc.Run: %v\n", err)
		}
		return
	}

	ServiceMode = false
	StartService()
}
