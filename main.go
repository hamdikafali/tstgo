package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	workerCount     = 1000  // İşçi sayısı
	bufferSize      = 10000 // Bellek tamponu boyutu (10.000 log)
	maxConnections  = 10000 // Maksimum TCP bağlantı sayısı
	logDir          = "logs"
)

var logChannel = make(chan LogEntry, bufferSize)
var epsCounter int
var mu sync.Mutex

type LogEntry struct {
	IP        string
	LogType   string
	Message   string
	Timestamp time.Time
}

func main() {
	var wg sync.WaitGroup

	// EPS sayaç fonksiyonunu başlat
	go monitorEPS()

	// İşçi havuzunu başlat
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go worker(&wg)
	}

	// TCP ve UDP dinleyicilerini başlat
	wg.Add(1)
	go func() {
		defer wg.Done()
		listenUDP("0.0.0.0:514")
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		listenTCP("0.0.0.0:514")
	}()

	fmt.Println("Log dinleme başlatıldı.")
	wg.Wait()
	close(logChannel)
}

// EPS monitörü
func monitorEPS() {
	ticker := time.NewTicker(1 * time.Second)
	for range ticker.C {
		mu.Lock()
		fmt.Printf("Anlık EPS: %d\n", epsCounter)
		epsCounter = 0
		mu.Unlock()
	}
}

// İşçi fonksiyonu
func worker(wg *sync.WaitGroup) {
	defer wg.Done()

	for logEntry := range logChannel {
		saveLog(logEntry)
	}
}

// UDP log dinleme fonksiyonu
func listenUDP(address string) {
	conn, err := net.ListenPacket("udp", address)
	if err != nil {
		fmt.Fprintf(os.Stderr, "UDP dinleme hatası: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	buffer := make([]byte, 8192)
	for {
		n, addr, err := conn.ReadFrom(buffer)
		if err != nil {
			fmt.Fprintf(os.Stderr, "UDP log alımı sırasında hata: %v\n", err)
			continue
		}

		processLog(addr, buffer[:n])
	}
}

// TCP log dinleme fonksiyonu
func listenTCP(address string) {
	ln, err := net.Listen("tcp", address)
	if err != nil {
		fmt.Fprintf(os.Stderr, "TCP dinleme hatası: %v\n", err)
		os.Exit(1)
	}
	defer ln.Close()

	semaphore := make(chan struct{}, maxConnections)

	for {
		conn, err := ln.Accept()
		if err != nil {
			fmt.Fprintf(os.Stderr, "TCP bağlantı hatası: %v\n", err)
			continue
		}

		semaphore <- struct{}{}
		go func() {
			handleTCPConnection(conn)
			<-semaphore
		}()
	}
}

// TCP bağlantısını işleyen fonksiyon
func handleTCPConnection(conn net.Conn) {
	defer conn.Close()

	buffer := make([]byte, 8192)
	for {
		n, err := conn.Read(buffer)
		if err != nil {
			if err.Error() != "EOF" {
				fmt.Fprintf(os.Stderr, "TCP log alımı sırasında hata: %v\n", err)
			}
			return
		}

		processLog(conn.RemoteAddr(), buffer[:n])
	}
}

// Logları işleyen fonksiyon
func processLog(addr net.Addr, data []byte) {
	ip := strings.Split(addr.String(), ":")[0]
	logMessage := string(data)
	logType := determineLogType(logMessage)

	logChannel <- LogEntry{
		IP:        ip,
		LogType:   logType,
		Message:   logMessage,
		Timestamp: time.Now(),
	}

	// EPS sayacını artır
	mu.Lock()
	epsCounter++
	mu.Unlock()
}

// Log türünü belirler
func determineLogType(message string) string {
	message = strings.ToLower(message)
	switch {
	case strings.Contains(message, "firewall"):
		return "firewall"
	case strings.Contains(message, "dhcp"):
		return "dhcp"
	case strings.Contains(message, "hotspot"):
		return "hotspot"
	default:
		return "general"
	}
}

func saveLog(entry LogEntry) {
	dateFolder := entry.Timestamp.Format("02-01-2006") // gün-ay-yıl formatı

	// logs/IP/gün-ay-yıl/logturu.log yapısını oluştur
	folderPath := filepath.Join(logDir, entry.IP, dateFolder)
	err := os.MkdirAll(folderPath, 0755)
	if err != nil {
		logError(err, "Klasör oluşturulamadı")
		return
	}

	// Günlük log dosyası ismi
	logFilePath := filepath.Join(folderPath, fmt.Sprintf("%s-%s.log", entry.LogType, entry.Timestamp.Format("02-01-2006")))

	// Dosya açma ve yazma denemesi
	for i := 0; i < 3; i++ { // Yeniden deneme sayısı
		file, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			logError(err, "Log dosyası açılamadı")
			time.Sleep(2 * time.Second) // 2 saniye bekleme
			continue
		}
		defer file.Close()

		_, err = file.WriteString(fmt.Sprintf("%s\n", entry.Message))
		if err != nil {
			logError(err, "Mesaj yazılamadı")
			time.Sleep(2 * time.Second) // 2 saniye bekleme
			continue
		}
		break // Başarılı olursa döngüden çık
	}
}

// Hataları loglayan fonksiyon
func logError(err error, context string) {
	errorLogPath := filepath.Join(logDir, "error.log")
	file, _ := os.OpenFile(errorLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	defer file.Close()

	timestamp := time.Now().Format("2006-01-02 15:04:05")
	file.WriteString(fmt.Sprintf("[%s] %s: %v\n", timestamp, context, err))
}
