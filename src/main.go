package main

import (
	"flag"
	"log"
	"net/http"
	"time" // Import time for duration parsing
	
	"san-exporter/configs"
	"san-exporter/collector"
)

func main() {
	// --- 1. Define Command-Line Flags ---
	
	// Define -web.listen-address flag
	listenAddress := flag.String("web.listen-address", "9111", "Address on which to expose metrics and web interface.",)
	
	// Define -config.file flag
	configFile := flag.String("config.file", "configs/config.yaml", "Path to the base configuration file.",)

	// Define -cache.interval flag (The new argument!)
	cacheInterval := flag.String("cache.interval", 
		"30s", // Default interval: 30 seconds
		"Minimum duration between successful scrapes for any single target. Must be a Go time duration (e.g., 10s, 1m).",)

	// Parse the flags defined above
	flag.Parse()

	log.Println("[INFO]: Starting Prometheus Exporter...")
    
    // --- 2. Parse and Validate Cache Interval ---
    intervalDuration, err := time.ParseDuration(*cacheInterval)
    if err != nil {
        log.Fatalf("[ERROR]: Invalid duration format for -cache.interval: %v", err)
    }

	// 3. Load Configuration 
	cfg, err := config.LoadConfig(*configFile)
	if err != nil {
		log.Fatalf("[ERROR]: Failed to load configuration from %s: %v", *configFile, err)
	}

	// 4. Create the Exporter instance and pass the interval
    // NOTE: We need to update the Exporter struct to accept intervalDuration.
	exp := collector.NewExporter(cfg, intervalDuration) 
	
	// 5. Define HTTP Handlers
	http.HandleFunc("/metrics", collector.TargetScrapeHandler(exp))

	log.Printf("[INFO]: Listening on %s with cache interval %s", *listenAddress, intervalDuration)
	port := ":" + *listenAddress
	log.Fatal(http.ListenAndServe(port, nil))
}