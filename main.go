package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	maxBufferSize = 4096
	queueSize     = 1000
	maxRetries    = 3
)

type logSource struct {
	Name    string
	Pattern *regexp.Regexp
	Type    string
}

type logEntry struct {
	sourceIP string
	message  string
	source   logSource
}

var (
	logQueue   = make(chan logEntry, queueSize)
	wg         sync.WaitGroup
	processWg  sync.WaitGroup
	// Desteklenen log kaynakları
	logSources = []logSource{
		{
			Name:    "MikroTik Firewall",
			Pattern: regexp.MustCompile(`(?i)firewall`),
			Type:    "firewall",
		},
		{
			Name:    "MikroTik DHCP",
			Pattern: regexp.MustCompile(`(?i)dhcp`),
			Type:    "dhcp",
		},
		{
			Name:    "MikroTik Hotspot",
			Pattern: regexp.MustCompile(`(?i)hotspot`),
			Type:    "hotspot",
		},
		{
			Name:    "Cisco Firewall",
			Pattern: regexp.MustCompile(`(?i)cisco.*firewall`),
			Type:    "firewall",
		},
		{
			Name:    "Fortinet UTM",
			Pattern: regexp.MustCompile(`(?i)fortinet.*utm`),
			Type:    "utm",
		},
		{
			Name:    "Palo Alto Firewall",
			Pattern: regexp.MustCompile(`(?i)palo alto.*firewall`),
			Type:    "firewall",
		},
	}
)

// TCP bağlantıları işleme
func handleTCPConnection(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	for {
		message, err := reader.ReadString('\n')
		if err != nil {
			fmt.Println("TCP bağlantı kapandı:", err)
			return
		}
		processLog(message, conn.RemoteAddr().String())
	}
}

// UDP bağlantıları işleme
func handleUDPConnection(conn *net.UDPConn) {
	buffer := make([]byte, maxBufferSize)
	for {
		n, addr, err := conn.ReadFromUDP(buffer)
		if err != nil {
			fmt.Println("UDP bağlantı hatası:", err)
			continue
		}
		message := string(buffer[:n])
		processLog(message, addr.String())
	}
}

// Logları ayrıştırmak ve kuyruğa eklemek
func processLog(message, sourceIP string) {
	for _, source := range logSources {
		if source.Pattern.MatchString(message) {
			logQueue <- logEntry{
				sourceIP: sourceIP,
				message:  message,
				source:   source,
			}
			return
		}
	}
	// Tanımsız loglar için bir işlem yapılacaksa buraya ekleyebilirsiniz
	fmt.Printf("Bilinmeyen log kaynağı: %s\n", message)
}

// Log mesajlarını işleme
func processLogMessages() {
	defer processWg.Done()
	for entry := range logQueue {
		processLogMessage(entry)
	}
}

// Logları IP'ye, türüne ve tarihine göre işleme
func processLogMessage(entry logEntry) {
	ip := strings.Split(entry.sourceIP, ":")[0]

	today := time.Now().Format("2006-01-02")
	directory := filepath.Join("logs", ip, today)
	err := os.MkdirAll(directory, 0755)
	if err != nil {
		fmt.Println("Klasör oluşturulamadı:", err)
		return
	}

	filePath := filepath.Join(directory, entry.source.Type+".log")
	for i := 0; i < maxRetries; i++ {
		err = writeLogToFile(filePath, entry.message+"\n") // Mesaj sonuna yeni satır ekle
		if err == nil {
			break
		}
		fmt.Printf("Log dosyaya yazılamadı (deneme %d/%d): %v\n", i+1, maxRetries, err)
		time.Sleep(100 * time.Millisecond) // Retry arasında kısa bir bekleme
	}
}

// Dosyaya yazma işlemi
func writeLogToFile(filePath, message string) error {
	file, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer file.Close()

	writer := bufio.NewWriter(file)
	_, err = writer.WriteString(message)
	if err != nil {
		return err
	}

	err = writer.Flush()
	return err
}

func main() {
	processWg.Add(1)
	go processLogMessages()

	// TCP dinleyici başlatma
	tcpLn, err := net.Listen("tcp", ":514")
	if err != nil {
		fmt.Println("TCP dinleme başlatılamadı:", err)
		os.Exit(1)
	}
	defer tcpLn.Close()
	fmt.Println("TCP Log Listener 514 portundan dinliyor...")

	// UDP dinleyici başlatma
	udpAddr, err := net.ResolveUDPAddr("udp", ":514")
	if err != nil {
		fmt.Println("UDP adresi çözülemedi:", err)
		os.Exit(1)
	}

	udpLn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		fmt.Println("UDP dinleme başlatılamadı:", err)
		os.Exit(1)
	}
	defer udpLn.Close()
	fmt.Println("UDP Log Listener 514 portundan dinliyor...")

	// TCP bağlantıları kabul etmek için goroutine
	go func() {
		for {
			conn, err := tcpLn.Accept()
			if err != nil {
				fmt.Println("TCP bağlantı kabul edilemedi:", err)
				continue
			}
			wg.Add(1)
			go func() {
				handleTCPConnection(conn)
				wg.Done()
			}()
		}
	}()

	// UDP bağlantıları dinleme
	go func() {
		handleUDPConnection(udpLn)
	}()

	// Programın kapanmasını engellemek için sonsuz döngü
	select {}
}
