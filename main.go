
package main

import (
    "bufio"
    "compress/gzip"
    "context"
    "fmt"
    "io"
    "net"
    "os"
    "path/filepath"
    "regexp"
    "strings"
    "sync"
    "time"

    "github.com/elastic/go-elasticsearch/v8"
    "github.com/elastic/go-elasticsearch/v8/esapi"
    "github.com/elastic/go-elasticsearch/v8/esutil"
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
    logQueue  = make(chan logEntry, queueSize)
    wg        sync.WaitGroup
    processWg sync.WaitGroup
    esClient  *elasticsearch.Client
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

func initElasticsearch() error {
    var err error
    cfg := elasticsearch.Config{
        Addresses: []string{
            "http://localhost:9200",
        },
    }
    esClient, err = elasticsearch.NewClient(cfg)
    if err != nil {
        return fmt.Errorf("Elasticsearch istemcisi başlatılamadı: %v", err)
    }
    return nil
}

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
    fmt.Printf("Bilinmeyen log kaynağı: %s\n", message)
}

func processLogMessages() {
    defer processWg.Done()
    for entry := range logQueue {
        processLogMessage(entry)
    }
}

func processLogMessage(entry logEntry) {
    err := writeLogToFile(entry)
    if err != nil {
        fmt.Println("Log dosyaya kaydedilemedi:", err)
    }

    go func() {
        err := indexLogToElasticsearch(entry)
        if err != nil {
            fmt.Println("Elasticsearch'e log gönderilemedi:", err)
        }
    }()
}

func writeLogToFile(entry logEntry) error {
    ip := strings.Split(entry.sourceIP, ":")[0]
    today := time.Now().Format("2006-01-02")
    directory := filepath.Join("logs", ip, today)
    err := os.MkdirAll(directory, 0755)
    if err != nil {
        return fmt.Errorf("Klasör oluşturulamadı: %v", err)
    }

    filePath := filepath.Join(directory, entry.source.Type+".log")
    file, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
    if err != nil {
        return err
    }
    defer file.Close()

    writer := bufio.NewWriter(file)
    _, err = writer.WriteString(entry.message + "\n")
    if err != nil {
        return err
    }

    return writer.Flush()
}

func indexLogToElasticsearch(entry logEntry) error {
    data := map[string]interface{}{
        "source_ip": entry.sourceIP,
        "message":   entry.message,
        "source":    entry.source.Name,
        "type":      entry.source.Type,
        "timestamp": time.Now(),
    }
    req := esapi.IndexRequest{
        Index:      "logs",
        DocumentID: "",
        Body:       esutil.NewJSONReader(data),
        Refresh:    "true",
    }
    res, err := req.Do(context.Background(), esClient)
    if err != nil {
        return fmt.Errorf("Elasticsearch'e log gönderilemedi: %v", err)
    }
    defer res.Body.Close()
    if res.IsError() {
        return fmt.Errorf("Hata oluştu: %s", res.String())
    }
    return nil
}

func compressOldLogs() {
    yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
    baseDir := "logs"

    filepath.Walk(baseDir, func(path string, info os.FileInfo, err error) error {
        if err != nil {
            return err
        }

        if info.IsDir() {
            return nil
        }

        if strings.Contains(path, yesterday) && !strings.HasSuffix(path, ".gz") {
            err := gzipFile(path)
            if err != nil {
                fmt.Println("Dosya sıkıştırılamadı:", err)
            } else {
                os.Remove(path)
            }
        }
        return nil
    })
}

func gzipFile(filePath string) error {
    inFile, err := os.Open(filePath)
    if err != nil {
        return err
    }
    defer inFile.Close()

    outFile, err := os.Create(filePath + ".gz")
    if err != nil {
        return err
    }
    defer outFile.Close()

    gzipWriter := gzip.NewWriter(outFile)
    defer gzipWriter.Close()

    _, err = io.Copy(gzipWriter, inFile)
    if err != nil {
        return err
    }

    return nil
}

func main() {
    if err := initElasticsearch(); err != nil {
        fmt.Println("Elasticsearch başlatılamadı:", err)
        os.Exit(1)
    }

    processWg.Add(1)
    go processLogMessages()

    go func() {
        for {
            now := time.Now()
            nextMidnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
            time.Sleep(time.Until(nextMidnight))
            compressOldLogs()
        }
    }()

    tcpLn, err := net.Listen("tcp", ":514")
    if err != nil {
        fmt.Println("TCP dinleme başlatılamadı:", err)
        os.Exit(1)
    }
    defer tcpLn.Close()
    fmt.Println("TCP Log Listener 514 portundan dinliyor...")

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

    go func() {
        handleUDPConnection(udpLn)
    }()

    select {}
}
