package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
)

const (
	dbPath        = "ip_store.db"
	ipifyAPI      = "https://api.ipify.org?format=json"
	checkInterval = 10 * time.Second
	envKey        = "CHARON_P2P_EXTERNAL_HOSTNAME"
	retryInterval = 5 * time.Second
	httpTimeout   = 10 * time.Second
)

type IPResponse struct {
	IP string `json:"ip"`
}

func getCurrentIP() (string, error) {
	client := &http.Client{
		Timeout: httpTimeout,
	}

	log.Printf("Fetching current IP from %s...", ipifyAPI)
	resp, err := client.Get(ipifyAPI)
	if err != nil {
		return "", fmt.Errorf("network error while fetching IP: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("received non-200 status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %v", err)
	}

	var ipResp IPResponse
	if err := json.Unmarshal(body, &ipResp); err != nil {
		return "", fmt.Errorf("failed to parse response: %v", err)
	}

	if ipResp.IP == "" {
		return "", fmt.Errorf("received empty IP from API")
	}

	log.Printf("Successfully fetched current IP: %s", ipResp.IP)
	return ipResp.IP, nil
}

func initDB() (*sql.DB, error) {
	log.Printf("Initializing SQLite database at %s...", dbPath)
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %v", err)
	}

	createTable := `
	CREATE TABLE IF NOT EXISTS ip_store (
		id INTEGER PRIMARY KEY,
		ip TEXT NOT NULL,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);`

	if _, err := db.Exec(createTable); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create table: %v", err)
	}

	log.Printf("Database initialized successfully")
	return db, nil
}

func getEnvIP() (string, error) {
	if err := godotenv.Load(); err != nil {
		return "", fmt.Errorf("failed to load .env file: %v", err)
	}

	ip := os.Getenv(envKey)
	if ip == "" {
		return "", fmt.Errorf("IP not found in .env file")
	}

	return ip, nil
}

func restartCharon() error {
	log.Printf("Restarting Charon container...")
	cmd := exec.Command("docker", "compose", "up", "charon", "-d", "--force-recreate")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to restart Charon: %v, output: %s", err, string(output))
	}
	log.Printf("Successfully restarted Charon container")
	return nil
}

func updateEnvFile(newIP string) error {
	log.Printf("Updating .env file with new IP: %s", newIP)
	input, err := os.ReadFile(".env")
	if err != nil {
		return fmt.Errorf("failed to read .env file: %v", err)
	}

	lines := strings.Split(string(input), "\n")
	found := false

	for i, line := range lines {
		if strings.HasPrefix(line, envKey+"=") {
			oldIP := strings.TrimPrefix(line, envKey+"=")
			lines[i] = fmt.Sprintf("%s=%s", envKey, newIP)
			found = true
			log.Printf("Updating IP in .env file: %s -> %s", oldIP, newIP)
			break
		}
	}

	if !found {
		log.Printf("No existing IP entry found in .env file, adding new entry")
		lines = append(lines, fmt.Sprintf("%s=%s", envKey, newIP))
	}

	output := strings.Join(lines, "\n")
	if err := os.WriteFile(".env", []byte(output), 0644); err != nil {
		return fmt.Errorf("failed to write .env file: %v", err)
	}

	log.Printf("Successfully updated .env file")

	if err := restartCharon(); err != nil {
		return fmt.Errorf("failed to restart Charon after IP update: %v", err)
	}

	return nil
}

func main() {
	log.Printf("Starting IP monitoring service...")
	log.Printf("Check interval: %v", checkInterval)

	db, err := initDB()
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer db.Close()

	log.Printf("IP monitoring service started successfully")
	log.Printf("Monitoring IP changes...")

	consecutiveErrors := 0
	maxConsecutiveErrors := 5

	for {
		currentIP, err := getCurrentIP()
		if err != nil {
			consecutiveErrors++
			log.Printf("Error getting current IP (attempt %d/%d): %v", consecutiveErrors, maxConsecutiveErrors, err)

			if consecutiveErrors >= maxConsecutiveErrors {
				log.Printf("Multiple consecutive errors detected. Increasing retry interval...")
				time.Sleep(checkInterval * 2) // Double the wait time after multiple failures
			} else {
				time.Sleep(retryInterval)
			}
			continue
		}
		consecutiveErrors = 0 // Reset error counter on successful IP fetch

		// Check if .env and DB are in sync
		envIP, err := getEnvIP()
		if err != nil {
			log.Printf("Warning: Could not get IP from .env: %v", err)
		}

		var storedIP string
		err = db.QueryRow("SELECT ip FROM ip_store ORDER BY updated_at DESC LIMIT 1").Scan(&storedIP)
		if err == sql.ErrNoRows {
			log.Printf("No IP found in database, storing first IP: %s", currentIP)
		} else if err != nil {
			log.Printf("Error querying database: %v", err)
			log.Printf("Will retry database query in %v...", retryInterval)
			time.Sleep(retryInterval)
			continue
		} else {
			log.Printf("Current stored IP: %s", storedIP)
		}

		// Update if: no IP in DB, IP changed, or .env is out of sync
		if err == sql.ErrNoRows ||
			(err == nil && storedIP != currentIP) ||
			(envIP != "" && envIP != storedIP) {

			if err := updateEnvFile(currentIP); err != nil {
				log.Printf("Error updating .env file: %v", err)
				log.Printf("Retrying in %v...", retryInterval)
				time.Sleep(retryInterval)
				continue
			}

			_, err = db.Exec("INSERT INTO ip_store (ip) VALUES (?)", currentIP)
			if err != nil {
				log.Printf("Error storing IP in database: %v", err)
			} else {
				log.Printf("Successfully stored new IP in database: %s", currentIP)
			}
		} else {
			log.Printf("No IP change detected. Current IP: %s", currentIP)
		}

		log.Printf("Waiting %v before next check...", checkInterval)
		time.Sleep(checkInterval)
	}
}
