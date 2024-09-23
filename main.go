package main

import (
    "archive/zip"
    "bytes"
    "crypto"
    "crypto/rand"
    "crypto/rsa"
    "crypto/sha256"
    "crypto/tls"
    "encoding/json"
    "errors"
    "fmt"
    "io"
    "net/http"
    "os"
    "path/filepath"
    "time"

    "github.com/digitorus/timestamp"
    "gopkg.in/yaml.v2"
)

// Yapılandırma dosyasını temsil eden yapı
type Config struct {
    LogDirectory string `yaml:"log_dir"`
    CertPath     string `yaml:"cert_path"`
    KeyPath      string `yaml:"key_path"`
}

// EpsLimits yapılandırmasını temsil eden yapı
type EpsLimits struct {
    Limits map[string]struct {
        Threshold int `json:"threshold"`
    } `json:"limits"`
}

// YAML dosyasını okuma
func readYAMLConfig(configPath string) (*Config, error) {
    data, err := os.ReadFile(configPath)
    if err != nil {
        return nil, err
    }

    var config Config
    err = yaml.Unmarshal(data, &config)
    if err != nil {
        return nil, err
    }

    return &config, nil
}

// JSON dosyasını okuma
func readJSONConfig(jsonPath string) (*EpsLimits, error) {
    data, err := os.ReadFile(jsonPath)
    if err != nil {
        return nil, err
    }

    var epsLimits EpsLimits
    err = json.Unmarshal(data, &epsLimits)
    if err != nil {
        return nil, err
    }

    return &epsLimits, nil
}

// Sertifikayı ve özel anahtarı yükleme
func loadCertificate(certPath, keyPath string) (tls.Certificate, error) {
    cert, err := tls.LoadX509KeyPair(certPath, keyPath)
    if err != nil {
        return tls.Certificate{}, err
    }
    return cert, nil
}

// Log dosyasını imzalama
func signLogFile(data []byte, privateKey *rsa.PrivateKey) ([]byte, error) {
    hash := sha256.Sum256(data)
    signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, hash[:])
    if err != nil {
        return nil, err
    }
    return signature, nil
}

// Zaman damgası alma
func getTimestampToken(hash []byte) ([]byte, error) {
    req, err := timestamp.CreateRequest(bytes.NewReader(hash), &timestamp.RequestOptions{
        Hash:         crypto.SHA256,
        Certificates: true,
    })
    if err != nil {
        return nil, err
    }

    tsaURL := "https://freetsa.org/tsr"

    client := &http.Client{}
    tsResp, err := timestamp.SendRequest(client, tsaURL, req)
    if err != nil {
        return nil, err
    }

    return tsResp.RawToken, nil
}

// İmza ve zaman damgasını birleştirme
type SignedData struct {
    Signature      []byte `json:"signature"`
    TimestampToken []byte `json:"timestamp_token"`
}

func saveSignedData(filePath string, data SignedData) error {
    fileData, err := json.Marshal(data)
    if err != nil {
        return err
    }
    return os.WriteFile(filePath, fileData, 0644)
}

// İmza doğrulama
func verifySignature(data, signature []byte, publicKey *rsa.PublicKey) error {
    hash := sha256.Sum256(data)
    return rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, hash[:], signature)
}

// Zaman damgası doğrulama
func verifyTimestampToken(hash, token []byte) error {
    resp, err := timestamp.ParseResponse(token)
    if err != nil {
        return err
    }

    if !bytes.Equal(resp.Hash(), hash) {
        return errors.New("Zaman damgası hash ile eşleşmiyor")
    }

    return nil
}

func main() {
    // 1. YAML dosyasını oku
    config, err := readYAMLConfig("config.yaml")
    if err != nil {
        fmt.Println("YAML dosyası okunamadı:", err)
        return
    }

    // Sertifikayı ve özel anahtarı yükle
    cert, err := loadCertificate(config.CertPath, config.KeyPath)
    if err != nil {
        fmt.Println("Sertifika veya özel anahtar yüklenemedi:", err)
        return
    }

    privateKey, ok := cert.PrivateKey.(*rsa.PrivateKey)
    if !ok {
        fmt.Println("Özel anahtar RSA tipi değil")
        return
    }

    // 2. JSON dosyasını oku
    epsLimits, err := readJSONConfig("eps_limits.json")
    if err != nil {
        fmt.Println("JSON dosyası okunamadı:", err)
        return
    }

    // 3. Bir gün öncesinin tarihini bul
    yesterday := time.Now().AddDate(0, 0, -1)
    yesterdayStr := yesterday.Format("02-01-2006")

    // 4. IP adreslerini dolaşarak log dizinlerini bul ve işle
    for ip := range epsLimits.Limits {
        logPath := filepath.Join(config.LogDirectory, ip, yesterdayStr) // logs/ip/tarih

        // Zip dosyasının adını günün tarihi şeklinde oluşturuyoruz
        zipFile := filepath.Join(logPath, yesterdayStr+".zip")

        // Dizin içerisindeki tüm dosyaları sıkıştırma
        files, err := os.ReadDir(logPath)
        if err != nil {
            fmt.Printf("Log dizini okunamadı (%s): %v\n", logPath, err)
            continue
        }

        zipArchive, err := os.Create(zipFile)
        if err != nil {
            fmt.Printf("Zip dosyası oluşturulamadı (%s): %v\n", zipFile, err)
            continue
        }
        defer zipArchive.Close()

        archive := zip.NewWriter(zipArchive)
        defer archive.Close()

        for _, file := range files {
            if file.IsDir() {
                continue // Dizinin içindeki alt klasörleri sıkıştırmıyoruz
            }

            // Dosya yolunu oluşturuyoruz
            filePath := filepath.Join(logPath, file.Name())

            // Dosyayı sıkıştırıyoruz
            fileToZip, err := os.Open(filePath)
            if err != nil {
                fmt.Printf("Dosya açılamadı (%s): %v\n", filePath, err)
                continue
            }
            defer fileToZip.Close()

            // Zip dosyasına eklemek için zip writer oluşturuyoruz
            wr, err := archive.Create(file.Name())
            if err != nil {
                fmt.Printf("Zip'e dosya eklenemedi (%s): %v\n", filePath, err)
                continue
            }

            // Dosyayı zip'e yazıyoruz
            _, err = io.Copy(wr, fileToZip)
            if err != nil {
                fmt.Printf("Dosya zip'e yazılamadı (%s): %v\n", filePath, err)
                continue
            }
        }

        fmt.Println("Tüm dosyalar başarıyla sıkıştırıldı:", zipFile)

        // Sıkıştırılan dosyayı oku
        zipData, err := os.ReadFile(zipFile)
        if err != nil {
            fmt.Println("Sıkıştırılan dosya okunamadı:", err)
            continue
        }

        // Dosyayı imzala
        signature, err := signLogFile(zipData, privateKey)
        if err != nil {
            fmt.Println("Dosya imzalanamadı:", err)
            continue
        }

        // Hash'i oluştur (zaman damgası için)
        hash := sha256.Sum256(zipData)

        // Zaman damgası al
        timestampToken, err := getTimestampToken(hash[:])
        if err != nil {
            fmt.Println("Zaman damgası alınamadı:", err)
            continue
        }

        // İmza ve zaman damgasını kaydet
        signedData := SignedData{
            Signature:      signature,
            TimestampToken: timestampToken,
        }

        signedFile := filepath.Join(logPath, "logfile-signed.json")
        err = saveSignedData(signedFile, signedData)
        if err != nil {
            fmt.Println("İmzalanan dosya kaydedilemedi:", err)
            continue
        }

        fmt.Println("Log dosyası başarıyla imzalandı ve zaman damgası eklendi:", signedFile)
    }
}
