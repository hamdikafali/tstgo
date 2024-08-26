
package main

import (
    "encoding/json"
    "fmt"
    "io/ioutil"
    "net"
    "os"
    "path/filepath"
    "strings"
    "sync"
    "time"
)

const (
    initialWorkerCount = 10    // Başlangıç işçi sayısı
    maxWorkerCount     = 100   // Maksimum işçi sayısı
    bufferSize         = 10000 // Bellek tamponu boyutu (10.000 log)
    maxConnections     = 10000 // Maksimum TCP bağlantı sayısı
    logDir             = "logs"
    configFile         = "config.json" // Konfigürasyon dosyası
	epsConfigFile 	   = "eps_limits.json"
)

var (
	ipEPS       = make(map[string]int)   // Her IP için EPS sayacı
	ipLimits    = make(map[string]int)   // Her IP için EPS limiti
    logChannel   = make(chan LogEntry, bufferSize)
    workerCount  = initialWorkerCount
    workerMutex  sync.Mutex
    epsCounter   int
    mu           sync.Mutex
	config      EPSConfig
)

type LogEntry struct {
    IP        string
    LogType   string
    Message   string
    Timestamp time.Time
    Device    string // Logun geldiği cihazı belirtiyor
}

// Config struct to hold the configuration from the JSON file
type Config struct {
    Devices  map[string]string   `json:"devices"`
    LogTypes map[string][]string `json:"log_types"`
}

// EPSConfig struct to hold the EPS limits from the JSON file
type EPSConfig struct {
	Limits map[string]int `json:"limits"`
}

func main() {
    var wg sync.WaitGroup

    // EPS Konfigürasyon dosyasını yükle
    err := loadEPSConfig(epsConfigFile)
    if err != nil {
        fmt.Fprintf(os.Stderr, "EPS Konfigürasyon yüklenirken hata: %v\n", err)
        os.Exit(1)
    }

    // İşçi havuzunu başlat
    for i := 0; i < initialWorkerCount; i++ {
        wg.Add(1)
        go worker(&wg)
    }

    // EPS monitörünü başlat
    go monitorEPS()

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

    // İşçi havuzunu dinamik yönet
    go manageWorkers()

    fmt.Println("Log dinleme başlatıldı.")
    wg.Wait()
    close(logChannel)
}

// EPS Konfigürasyon dosyasını yükler
func loadEPSConfig(file string) error {
    data, err := ioutil.ReadFile(file)
    if err != nil {
        return err
    }
    return json.Unmarshal(data, &config)
}

// Belirtilen IP için EPS limitini döndürür
func getEPSLimit(ip string) int {
	if limit, exists := config.Limits[ip]; exists {
		return limit
	}
	return config.Limits["default"]
}


// Konfigürasyon dosyasını yükler
func loadConfig(file string) error {
    data, err := ioutil.ReadFile(file)
    if err != nil {
        return err
    }
    return json.Unmarshal(data, &config)
}

// EPS monitörü
func monitorEPS() {
	ticker := time.NewTicker(1 * time.Second)
	for range ticker.C {
		mu.Lock()
		for ip, count := range ipEPS {
			limit := getEPSLimit(ip)
			//fmt.Printf("Zaman: %s, IP: %s, EPS: %d, Limit: %d\n", time.Now().Format("2006-01-02 15:04:05"), ip, count, limit)
			if count > limit {
				fmt.Printf("ALERT! IP %s EPS değeri limiti aştı: %d\n", ip, count)
			}
			ipEPS[ip] = 0 // EPS sayacını sıfırla
		}
		mu.Unlock()
	}
}


// EPS monitörü
func monitorEPS() {
	ticker := time.NewTicker(1 * time.Second)
	for range ticker.C {
		mu.Lock()
		for ip, count := range ipEPS {
			fmt.Printf("Zaman: %s, IP: %s, EPS: %d\n", time.Now().Format("2006-01-02 15:04:05"), ip, count)
			ipEPS[ip] = 0 // EPS sayacını sıfırla
		}
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

    device := determineDevice(logMessage)
    logType := determineLogType(logMessage)

    logChannel <- LogEntry{
        IP:        ip,
        LogType:   logType,
        Message:   logMessage,
        Timestamp: time.Now(),
        Device:    device,
    }

    // EPS sayacını artır
    mu.Lock()
	ipEPS[ip]++
    epsCounter++
    mu.Unlock()
}

// Cihazı belirler
func determineDevice(message string) string {
    for device, keyword := range config.Devices {
        if strings.Contains(strings.ToLower(message), strings.ToLower(keyword)) {
            return device
        }
    }
    return "unknown"
}

// Log türünü belirler
func determineLogType(message string) string {
    message = strings.ToLower(message)
    for logType, keywords := range config.LogTypes {
        for _, keyword := range keywords {
            if strings.Contains(message, keyword) {
                return logType
            }
        }
    }
    return "general"
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
