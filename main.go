package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v2"
)

const (
	epsConfigFile      = "eps_limits.json" // EPS limitlerini içeren JSON dosyası
	initialWorkerCount = 10                // Başlangıç işçi sayısı
	maxWorkerCount     = 100               // Maksimum işçi sayısı
	bufferSize         = 10000             // Bellek tamponu boyutu (10.000 log)
	maxConnections     = 10000             // Maksimum TCP bağlantı sayısı
	SecurityToken      = "Karecode2025!!"  // Manuel olarak ayarlanan güvenlik tokeni
)

var (
	logChannel      = make(chan LogEntry, bufferSize)
	workerCount     = initialWorkerCount
	workerMutex     sync.Mutex
	mu              sync.Mutex
	epsConfig       EPSConfig
	ipEPS           = make(map[string][]time.Time) // Her IP'nin log zamanlarını saklayacağız
	yamlConfig      Config
	yamlConfigMutex sync.RWMutex
)

type LogEntry struct {
	IP        string
	LogType   string
	Message   string
	Timestamp time.Time
}

// Config struct to hold the configuration from config.yaml
type Config struct {
	EPSCheckInterval int    `yaml:"eps_check_interval"`
	LogInterval      int    `yaml:"log_interval"`  // Log dosyasına EPS yazma sıklığı (dakika cinsinden)
	PostInterval     int    `yaml:"post_interval"` // POST işlemi sıklığı (dakika cinsinden)
	PostURL          string `yaml:"post_url"`
	IP               string `yaml:"ip"`
	Customer         string `yaml:"musteri"`
	ID               string `yaml:"id"`
	LogDir           string `yaml:"log_dir"` // Log dizini config.yaml'dan alınacak
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
		return fmt.Errorf("config.yaml dosyası yüklenirken hata: %v", err)
	}

	var newConfig Config
	err = yaml.Unmarshal(data, &newConfig)
	if err != nil {
		return fmt.Errorf("YAML dosyası parse edilirken hata: %v", err)
	}

	// Config değerlerini kontrol et
	if newConfig.EPSCheckInterval <= 0 {
		return fmt.Errorf("config.yaml dosyasında 'eps_check_interval' eksik veya geçersiz.")
	}
	if newConfig.LogInterval <= 0 {
		return fmt.Errorf("config.yaml dosyasında 'log_interval' eksik veya geçersiz.")
	}
	if newConfig.PostInterval <= 0 {
		return fmt.Errorf("config.yaml dosyasında 'post_interval' eksik veya geçersiz.")
	}
	if newConfig.PostURL == "" {
		return fmt.Errorf("config.yaml dosyasında 'post_url' eksik.")
	}
	if newConfig.IP == "" {
		return fmt.Errorf("config.yaml dosyasında 'ip' eksik.")
	}
	if newConfig.Customer == "" {
		return fmt.Errorf("config.yaml dosyasında 'musteri' eksik.")
	}
	if newConfig.ID == "" {
		return fmt.Errorf("config.yaml dosyasında 'id' eksik.")
	}
	if newConfig.LogDir == "" {
		return fmt.Errorf("config.yaml dosyasında 'log_dir' eksik.")
	}

	yamlConfigMutex.Lock()
	yamlConfig = newConfig
	yamlConfigMutex.Unlock()

	return nil
}

func main() {
	var wg sync.WaitGroup

	// YAML dosyasını ilk başta yükle
	err := loadYAMLConfig()
	if err != nil {
		logToFile(fmt.Sprintf("YAML Konfigürasyon yüklenirken hata: %v\n", err))
		os.Exit(1)
	}

	logToFile("YAML Konfigürasyon yüklendi.")

	// EPS Konfigürasyon dosyasını yükle
	err = loadEPSConfig(epsConfigFile)
	if err != nil {
		logToFile(fmt.Sprintf("EPS Konfigürasyon yüklenirken hata: %v\n", err))
		os.Exit(1)
	}
	logToFile("EPS Konfigürasyon yüklendi.")

	// Worker havuzunu başlat
	for i := 0; i < initialWorkerCount; i++ {
		wg.Add(1)
		go worker(&wg)
	}
	logToFile("Workerlar Devreye Alındı.")

	// EPS monitörünü başlat
	go monitorEPS()

	// TCP ve UDP dinleyicilerini başlat
	wg.Add(2)
	go func() {
		defer wg.Done()
		listenUDP("0.0.0.0:514")
	}()
	logToFile("UDP Dinlemeye Başlandı.")

	go func() {
		defer wg.Done()
		listenTCP("0.0.0.0:514")
	}()
	logToFile("TCP Dinlemeye Başlandı.")

	// Worker havuzunu dinamik yönet
	go manageWorkers()

	// EPS verilerini log dosyasına belirlenen aralıklarla yazdır
	go logEPSDataPeriodically()

	// Belirli aralıklarla POST işlemi yap
	go sendPeriodicPost()

	logToFile("Log Sistemi Aktif Başladı.")
	wg.Wait()
	close(logChannel)
}

// EPS Konfigürasyon dosyasını yükler
func loadEPSConfig(file string) error {
	data, err := ioutil.ReadFile(file)
	if err != nil {
		return fmt.Errorf("eps_limits.json dosyası yüklenirken hata: %v", err)
	}
	err = json.Unmarshal(data, &epsConfig)
	if err != nil {
		return fmt.Errorf("eps_limits.json dosyası parse edilirken hata: %v", err)
	}

	// EPS limits yapılandırmasını kontrol et
	if len(epsConfig.Limits) == 0 {
		return fmt.Errorf("eps_limits.json dosyasında 'limits' alanı boş.")
	}

	return nil
}

// Dinamik işçi yönetimi
func manageWorkers() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		workerMutex.Lock()
		currentLogLen := len(logChannel)
		if currentLogLen > bufferSize/2 && workerCount < maxWorkerCount {
			workerCount++
			logToFile(fmt.Sprintf("Worker sayısı artırıldı: %d\n", workerCount))
			go worker(nil) // Yeni worker başlat
		} else if currentLogLen < bufferSize/4 && workerCount > initialWorkerCount {
			workerCount--
			logToFile(fmt.Sprintf("Worker sayısı azaltıldı: %d\n", workerCount))
			// Worker'ları durdurmak için eklemeler yapılabilir
		}
		workerMutex.Unlock()
	}
}

// EPS monitörü
func monitorEPS() {
	ticker := time.NewTicker(time.Duration(yamlConfig.EPSCheckInterval) * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		mu.Lock()
		for ip := range ipEPS {
			ipEPS[ip] = append(ipEPS[ip], time.Now()) // IP için log zamanını ekle
		}
		mu.Unlock()
	}
}

// Worker fonksiyonu
func worker(wg *sync.WaitGroup) {
	for logEntry := range logChannel {
		saveLog(logEntry)
	}
	if wg != nil {
		wg.Done()
	}
}

// UDP log dinleme fonksiyonu
func listenUDP(address string) {
	conn, err := net.ListenPacket("udp", address)
	if err != nil {
		logToFile(fmt.Sprintf("UDP dinleme hatası: %v\n", err))
		os.Exit(1)
	}
	defer conn.Close()

	buffer := make([]byte, 8192)
	for {
		n, addr, err := conn.ReadFrom(buffer)
		if err != nil {
			logToFile(fmt.Sprintf("UDP log alımı sırasında hata: %v\n", err))
			continue
		}

		processLog(addr, buffer[:n])
	}
}

// TCP log dinleme fonksiyonu
func listenTCP(address string) {
	ln, err := net.Listen("tcp", address)
	if err != nil {
		logToFile(fmt.Sprintf("TCP dinleme hatası: %v\n", err))
		os.Exit(1)
	}
	defer ln.Close()

	semaphore := make(chan struct{}, maxConnections)

	for {
		conn, err := ln.Accept()
		if err != nil {
			logToFile(fmt.Sprintf("TCP bağlantı hatası: %v\n", err))
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
				logToFile(fmt.Sprintf("TCP log alımı sırasında hata: %v\n", err))
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
		// IP adresi EPS limits dosyasında tanımlı değilse, bu bir "Korsan IP"
		logToFile(fmt.Sprintf("Korsan IP: %s\n", ip))
		return
	}

	logMessage := string(data)

	// EPS sayacını artır
	mu.Lock()
	ipEPS[ip] = append(ipEPS[ip], time.Now())
	mu.Unlock()

	// Log girişini kaydet
	logChannel <- LogEntry{
		IP:        ip,
		LogType:   determineLogType(),
		Message:   logMessage,
		Timestamp: time.Now(),
	}
}

// Log türünü belirler
func determineLogType() string {
	return "general"
}

// Logu dosyaya kaydet
func saveLog(entry LogEntry) {
	dateFolder := entry.Timestamp.Format("02-01-2006")

	// logs/IP/gün-ay-yıl/logturu.log yapısını oluştur
	folderPath := filepath.Join(yamlConfig.LogDir, entry.IP, dateFolder)
	err := os.MkdirAll(folderPath, 0755)
	if err != nil {
		logToFile(fmt.Sprintf("Klasör oluşturulamadı: %v\n", err))
		return
	}

	// Günlük log dosyası ismi
	logFilePath := filepath.Join(folderPath, fmt.Sprintf("%s-%s.log", entry.LogType, entry.Timestamp.Format("02-01-2006")))

	// Dosya açma ve yazma denemesi
	for i := 0; i < 3; i++ {
		file, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			logToFile(fmt.Sprintf("Log dosyası açılamadı: %v\n", err))
			time.Sleep(2 * time.Second)
			continue
		}
		defer file.Close()

		_, err = file.WriteString(fmt.Sprintf("%s\n", entry.Message))
		if err != nil {
			logToFile(fmt.Sprintf("Mesaj yazılamadı: %v\n", err))
			time.Sleep(2 * time.Second)
			continue
		}
		break
	}
}

// Hataları loglayan fonksiyon
func logError(err error, context string) {
	logToFile(fmt.Sprintf("%s: %v\n", context, err))
}

// Log mesajlarını dosyaya yazan fonksiyon
func logToFile(message string) {
	// Zaman damgasını oluştur
	timestamp := time.Now().Format("02-01-2006 15:04:05")

	// Log dosyasının ismini oluştur
	logFileName := time.Now().Format("kclogger-02-01-2006.log")
	logFilePath := filepath.Join(yamlConfig.LogDir, logFileName)

	// Klasör yoksa oluştur
	if err := os.MkdirAll(yamlConfig.LogDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Log klasörü oluşturulamadı: %v\n", err)
		return
	}

	// Mesajı dosyaya yaz
	logMessage := fmt.Sprintf("[%s] %s\n", timestamp, message) // Alt alta yazmak için yeni satır ekleniyor
	file, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Log dosyasına yazılamadı: %v\n", err)
		return
	}
	defer file.Close()

	if _, err := file.WriteString(logMessage); err != nil {
		fmt.Fprintf(os.Stderr, "Log dosyasına yazılamadı: %v\n", err)
	}
}

// EPS verilerini log dosyasına belirli aralıklarla yazdıran fonksiyon
func logEPSDataPeriodically() {
	for {
		yamlConfigMutex.RLock()
		logInterval := yamlConfig.LogInterval
		yamlConfigMutex.RUnlock()

		time.Sleep(time.Duration(logInterval) * time.Minute) // Log sıklığını yaml'dan alır

		mu.Lock()
		for ip, timestamps := range ipEPS {
			// Son log_interval dakikadaki log girişlerini filtrele
			cutoff := time.Now().Add(-time.Duration(logInterval) * time.Minute)
			var recentLogs int
			for _, t := range timestamps {
				if t.After(cutoff) {
					recentLogs++
				}
			}
			// Ortalama EPS hesapla
			eps := float64(recentLogs) / (float64(logInterval) * 60.0) // Saniyede ortalama EPS
			logToFile(fmt.Sprintf("IP: %s, Ortalama EPS (Son %d dakika): %.2f", ip, logInterval, eps))
		}
		mu.Unlock()
	}
}

// Periyodik POST gönderimi fonksiyonu
func sendPeriodicPost() {
	for {
		yamlConfigMutex.RLock()
		postInterval := yamlConfig.PostInterval
		postURL := yamlConfig.PostURL
		ip := yamlConfig.IP
		customer := yamlConfig.Customer
		id := yamlConfig.ID
		yamlConfigMutex.RUnlock()

		time.Sleep(time.Duration(postInterval) * time.Minute)

		// EPS verilerini JSON formatında POST isteği ile gönder
		sendEPSData(postURL, ip, customer, id)
	}
}

// EPS verilerini JSON formatında gönder
func sendEPSData(postURL,ip, customer, id string) {
    // IP'yi doğrudan YAML konfigürasyonundan alıyoruz
    //yamlConfigMutex.RLock()
   // ip := yamlConfig.IP
   // yamlConfigMutex.RUnlock()

    // EPS hesaplamalarını kaldırıyoruz ve EPS'yi göndermiyoruz

    // JSON verisine dönüştür
    payload := map[string]interface{}{
        "ip":       ip,
        "customer": customer,
        "id":       id,
        "token":    SecurityToken,
    }

    jsonData, err := json.Marshal(payload)
    if err != nil {
        logToFile(fmt.Sprintf("JSON verisi oluşturulurken hata: %v\n", err))
        return
    }

    req, err := http.NewRequest("POST", postURL, strings.NewReader(string(jsonData)))
    if err != nil {
        logToFile(fmt.Sprintf("POST isteği oluşturulurken hata: %v\n", err))
        return
    }

    req.Header.Set("Content-Type", "application/json")

    client := &http.Client{}
    resp, err := client.Do(req)
    if err != nil {
        logToFile(fmt.Sprintf("POST isteği gönderilirken hata: %v\n", err))
        return
    }
    defer resp.Body.Close()

    body, err := ioutil.ReadAll(resp.Body)
    if err != nil {
        logToFile(fmt.Sprintf("Yanıt okunurken hata: %v\n", err))
        return
    }

    logToFile(fmt.Sprintf("POST isteği gönderildi, yanıt: %s\n", string(body)))
}

