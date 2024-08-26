package main

import (
	"gopkg.in/yaml.v2"
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
    configFile         = "devices.json"      // Mevcut config.json
    epsConfigFile      = "eps_limits.json"  // EPS limitlerini içeren JSON dosyası
    initialWorkerCount = 10                 // Başlangıç işçi sayısı
    maxWorkerCount     = 100                // Maksimum işçi sayısı
    bufferSize         = 10000              // Bellek tamponu boyutu (10.000 log)
    maxConnections     = 10000              // Maksimum TCP bağlantı sayısı
    logDir             = "logs"
)

var (
    logChannel   = make(chan LogEntry, bufferSize)
    workerCount  = initialWorkerCount
    workerMutex  sync.Mutex
    epsCounter   int
    mu           sync.Mutex
    config       Config
    epsConfig    EPSConfig
	ipEPS = make(map[string]int)
	EPSCheckInterval int `yaml:"eps_check_interval"`
)
type LogEntry struct {
    IP        string
    LogType   string
    Message   string
    Timestamp time.Time
    Device    string // Logun geldiği cihazı belirtiyor
}

// Config struct for the main config.json
type Config struct {
    Devices  map[string]string   `json:"devices"`
    LogTypes map[string][]string `json:"log_types"`
}

// EPSConfig struct to hold the EPS limits from the JSON file
type EPSConfig struct {
    Limits map[string]IPConfig `json:"limits"`
}

// IPConfig struct for each IP's EPS limit and behavior
type IPConfig struct {
    Limit int  `json:"limit"`
    Alert bool `json:"alert"`
}

func loadYAMLConfig() error {
    data, err := ioutil.ReadFile("config.yaml")
    if err != nil {
        return err
    }
    return yaml.Unmarshal(data, &yamlConfig)
}
func main() {
    var wg sync.WaitGroup

    // Ana Konfigürasyon dosyasını yükle
    err := loadConfig(configFile)
    if err != nil {
        fmt.Println(os.Stderr, "Ana Konfigürasyon yüklenirken hata: %v\\n", err)
        os.Exit(1)
    }

    // EPS Konfigürasyon dosyasını yükle
    err = loadEPSConfig(epsConfigFile)
    if err != nil {
        fmt.Println(os.Stderr, "EPS Konfigürasyon yüklenirken hata: %v\\n", err)
        os.Exit(1)
    }

    // Konfigürasyon dosyasını düzenli aralıklarla kontrol et
    go monitorConfigFile()

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
// Ana Konfigürasyon dosyasını yükler
func loadConfig(file string) error {
    data, err := ioutil.ReadFile(file)
    if err != nil {
        return err
    }
    return json.Unmarshal(data, &config)
}

// EPS Konfigürasyon dosyasını yükler
func loadEPSConfig(file string) error {
    data, err := ioutil.ReadFile(file)
    if err != nil {
        return err
    }
    return json.Unmarshal(data, &epsConfig)
}

// Konfigürasyon dosyasını düzenli aralıklarla kontrol eder
func monitorConfigFile() {
    ticker := time.NewTicker(1 * time.Minute)
    for range ticker.C {
        err := loadEPSConfig(epsConfigFile)
        if err != nil {
            fmt.Println(os.Stderr, "EPS Konfigürasyon yeniden yüklenirken hata: %v\\n", err)
        } else {
            fmt.Println("EPS Konfigürasyonu yeniden yüklendi.")
        }
    }
}
// Dinamik işçi yönetimi
func manageWorkers() {
    ticker := time.NewTicker(5 * time.Second)
    for range ticker.C {
        workerMutex.Lock()
        if len(logChannel) > bufferSize/2 && workerCount < maxWorkerCount {
            workerCount++
            fmt.Printf("İşçi sayısı artırıldı: %d\n", workerCount)
        } else if len(logChannel) < bufferSize/4 && workerCount > initialWorkerCount {
            workerCount--
            fmt.PrintPrintfln("İşçi sayısı azaltıldı: %d\n", workerCount)
        }
        workerMutex.Unlock()
    }
}
// EPS monitörü
func monitorEPS() {
    ticker := time.NewTicker(1 * time.Second)
    for range ticker.C {
        mu.Lock()
        for ip, count := range ipEPS {
            ipConfig, exists := epsConfig.Limits[ip]
            if exists {
                if count > ipConfig.Limit {
                    if ipConfig.Alert {
                        fmt.Printf("ALERT! IP %s EPS değeri limiti aştı: %d\n", ip, count)
                    } else {
                        // Logu işleme almaz, bu durumu atlar
                        delete(ipEPS, ip)
                    }
                }
            } else {
                // IP adresi config dosyasında yoksa logu işleme alma
                delete(ipEPS, ip)
            }
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
        fmt.Println(os.Stderr, "UDP dinleme hatası: %v\\n", err)
        os.Exit(1)
    }
    defer conn.Close()

    buffer := make([]byte, 8192)
    for {
        n, addr, err := conn.ReadFrom(buffer)
        if err != nil {
            fmt.Println(os.Stderr, "UDP log alımı sırasında hata: %v\\n", err)
            continue
        }

        processLog(addr, buffer[:n])
    }
}
// TCP log dinleme fonksiyonu
func listenTCP(address string) {
    ln, err := net.Listen("tcp", address)
    if err != nil {
        fmt.Println(os.Stderr, "TCP dinleme hatası: %v\\n", err)
        os.Exit(1)
    }
    defer ln.Close()

    semaphore := make(chan struct{}, maxConnections)

    for {
        conn, err := ln.Accept()
        if err != nil {
            fmt.Println(os.Stderr, "TCP bağlantı hatası: %v\\n", err)
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
                fmt.Println(os.Stderr, "TCP log alımı sırasında hata: %v\\n", err)
            }
            return
        }

        processLog(conn.RemoteAddr(), buffer[:n])
    }
}
// Logları işleyen fonksiyon
func processLog(addr net.Addr, data []byte) {
    ip := strings.Split(addr.String(), ":")[0]

    // EPS limitleri arasında IP adresini kontrol et
    mu.Lock()
    _, exists := epsConfig.Limits[ip]
    mu.Unlock()
    if !exists {
        // IP adresi EPS limits dosyasında tanımlı değilse, logu işleme
        return
    }

    logMessage := string(data)

    // EPS sayacını artır
    mu.Lock()
    ipEPS[ip]++
    mu.Unlock()

    // Log girişini kaydet
    logChannel <- LogEntry{
        IP:        ip,
        LogType:   determineLogType(logMessage),
        Message:   logMessage,
        Timestamp: time.Now(),
        Device:    determineDevice(logMessage),
    }
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

        _, err = file.WriteString(fmt.Sprintf("%s\\n", entry.Message))
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
    file.WriteString(fmt.Sprintf("[%s] %s: %v\\n", timestamp, context, err))
}
