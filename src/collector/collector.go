package collector

import (
	"bytes"             // Needed for JSON body creation (bytes.NewBuffer)
	"crypto/tls"        // Needed for insecure HTTP client for HTTPS
	"encoding/json"     // Needed for JSON (un)marshalling in session methods
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"           // Needed for strings.ReplaceAll on the logout endpoint
	"sync"
	"time"
	
	config "san-exporter/configs" // ADJUST THIS IMPORT PATH to your module path
	
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/tidwall/gjson"
)

// Exporter holds the global configuration reference.
type Exporter struct {
	Config *config.AppConfig
	
	// CACHE FIELDS:
	CacheInterval time.Duration 			// The minimum time between scrapes
	LastScrape 	  map[string]time.Time 	// Stores last successful scrape time per target
	ScrapeData 	  map[string][]prometheus.Metric // Stores cached metrics per target
	CacheMutex 	  sync.RWMutex 			// Protects access to the cache maps
}

// TargetCollector holds the dynamic data for a single scrape, including credentials and session state.
type TargetCollector struct {
	Target 	 string
	Username string
	Password string
	Exporter *Exporter
	Client 	 *http.Client
    // UPDATED FIELDS: Separating the token used for authorization from the ID used for the URL path.
    SessionAuthToken string // Token used for the Authorization header: "Session {token}"
    SessionPathID string    // ID used for URL path substitution: "/sessions/{sessionid}"
}

func NewExporter(cfg *config.AppConfig, interval time.Duration) *Exporter {
	return &Exporter{
		Config: cfg,
		CacheInterval: interval,
		LastScrape: make(map[string]time.Time),
		ScrapeData: make(map[string][]prometheus.Metric),
		CacheMutex: sync.RWMutex{},
	}
}

// --- Prometheus Scrape Handling ---

// TargetScrapeHandler creates a TargetCollector for each incoming scrape request,
// executes Login/Scrape/Logout if configured, and serves the resulting metrics.
func TargetScrapeHandler(exp *Exporter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params := r.URL.Query()
		
		target := params.Get("target")
		if target == "" {
			http.Error(w, "Target parameter missing", http.StatusBadRequest)
			return
		}

		var username string
		var password string
		for _, mem := range exp.Config.Session {
			if mem.Name == "auth" {
				// return
				username = mem.Data["username"].(string)
				password = mem.Data["password"].(string) 
			}
			break
		}
		// Create an HTTP client that allows insecure skip verify (equivalent to curl -k) 
		// if the target is using HTTPS, which is common for internal APIs.
		client := http.DefaultClient
		if strings.HasPrefix(target, "https://") {
			client = &http.Client{
				Transport: &http.Transport{
					TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
				},
			}
		}

		// Create a new TargetCollector instance
		tc := &TargetCollector{
			Target: 	 target,
			Username: username,
			Password: password,
			Exporter: exp,
			Client: 	 client, // Use the configured client
            SessionAuthToken:  "", // Initialize
            SessionPathID:  "", // Initialize
		}
        
        // Check if session management is configured (i.e., if login endpoint exists)
        useSession := tc.findSessionRequest("login") != nil
        
        if useSession {
            // 1. LOGIN (Get Session ID/Token)
            log.Printf("[%s] [INFO]: Session-based authentication requested. Attempting login...", tc.Target)
			
            if err := tc.Login(); err != nil {
                log.Printf("[%s] [ERROR]: Login failed: %v", tc.Target, err)
                http.Error(w, fmt.Sprintf("Session login failed for %s: %v", tc.Target, err), http.StatusUnauthorized)
                return
            }
            
            // 2. LOGOUT (Ensure cleanup happens when this function exits)
            defer func() {
                // log.Printf("[%s] [INFO]: Attempting logout...", tc.Target)
                if err := tc.Logout(); err != nil {
                    log.Printf("[%s] [ERROR]: Logout cleanup failed: %v", tc.Target, err)
                } else {
				log.Printf("[%s] [INFO]: Logout succesfully.", tc.Target)
				}
            }()
        } else {
            log.Printf("[%s] [INFO]: Using Basic Authentication (No session config found).", tc.Target)
        }


		// 3. Register and Serve Metrics
		registry := prometheus.NewRegistry()
		registry.MustRegister(tc) // Register the TargetCollector instance
		
		h := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
		log.Printf("[%s] [INFO]: Starting metric collection...", tc.Target)
		h.ServeHTTP(w, r)
	}
}

// --- Session Management Methods ---

// findSessionRequest retrieves the request definition (login or logout) from config.
func (tc *TargetCollector) findSessionRequest(name string) *config.SessionRequest {
    for _, req := range tc.Exporter.Config.Session {
        if req.Name == name {
            return &req
        }
    }
    return nil
}

// performRequest executes the HTTP call defined in the SessionRequest
func (tc *TargetCollector) performRequest(reqDef *config.SessionRequest) (map[string]interface{}, error) {
    
    // Ensure the target URL starts with a protocol
	targetURL := tc.Target
	if !(len(targetURL) >= 7 && (targetURL[:7] == "http://" || targetURL[:8] == "https://")) {
		targetURL = "http://" + targetURL
	}
    
    // Replace placeholder in endpoint if SessionPathID is available
    endpoint := reqDef.Endpoint
    
    // Use SessionPathID for URL substitution
    if tc.SessionPathID != "" { 
        endpoint = strings.ReplaceAll(endpoint, "{sessionid}", tc.SessionPathID)
    }
    
    fullURL := targetURL + endpoint

    // --- Prepare Request Body and Authentication ---
    var requestBody io.Reader
    bodyData := reqDef.Data // Start with data from config

    if reqDef.Method == "POST" || reqDef.Method == "PUT" {
        if reqDef.Name == "login" {
            // LOGIN SPECIAL CASE: Per user's curl command (-d {}), the body must be an empty JSON object.
            // Authentication is done via Basic Auth, not in the body.
            
            // Override bodyData to be an empty map for marshaling
            bodyData = map[string]interface{}{}
            
        } 
        
        // Marshal the (possibly empty) bodyData
        body, err := json.Marshal(bodyData)
        if err != nil {
            return nil, fmt.Errorf("failed to marshal request body: %w", err)
        }
        requestBody = bytes.NewBuffer(body)
    }
    // --- End Body Preparation ---

    request, err := http.NewRequest(reqDef.Method, fullURL, requestBody)
    if err != nil {
        return nil, fmt.Errorf("failed to create request: %w", err)
    }
    request.Header.Set("Content-Type", "application/json")
    
    // --- Authentication Setting ---
    if reqDef.Name == "login" {
        // Apply Basic Auth for login as per curl command, using credentials from URL query params.
        request.SetBasicAuth(tc.Username, tc.Password)
        // log.Printf("[%s] [DEBUG]: Applying Basic Auth for login POST request to %s", tc.Target, fullURL)
    } else if tc.SessionAuthToken != "" {
        // If a session is active and this is not the login request, use the authentication token
        // Use "Session" prefix based on user's data curl command.
        request.Header.Set("Authorization", fmt.Sprintf("Session %s", tc.SessionAuthToken)) 
        // log.Printf("[%s] [DEBUG]: Applying Session Token for API call to %s", tc.Target, fullURL)
    }
    // --- End Authentication Setting ---

    // Execute the request
    resp, err := tc.Client.Do(request)
    if err != nil {
        return nil, fmt.Errorf("request failed: %w", err)
    }
    defer resp.Body.Close()

    // Check for success status codes
    if resp.StatusCode < 200 || resp.StatusCode >= 300 {
        return nil, fmt.Errorf("request failed with status: %s", resp.Status)
    }
    
    // Read and unmarshal response body
    responseBody, _ := io.ReadAll(resp.Body)
    var result map[string]interface{}
    if len(responseBody) > 0 && resp.StatusCode != http.StatusNoContent {
        if err := json.Unmarshal(responseBody, &result); err != nil {
             return nil, fmt.Errorf("failed to parse response body: %w, response: %s", err, string(responseBody))
        }
    }
    return result, nil
}

// Login attempts to establish a session for the target, using the provided Username/Password
// for the request body, and extracting the resulting session ID/token from the response.
func (tc *TargetCollector) Login() error {
	req := tc.findSessionRequest("login")
	if req == nil {
		return fmt.Errorf("login request configuration missing")
	}

	result, err := tc.performRequest(req)
	if err != nil {
		return fmt.Errorf("login request failed: %w", err)
	}

    // --- Token and ID Extraction Logic (Updated for case and type) ---
    
    // 1. Get Auth Token (Expected to be a string)
    authToken, okToken := result["token"].(string)
    
    // 2. Get Path ID (Expected to be a number/float64 under key "sessionId")
    var pathID string
    pathIDVal, okPathID := result["sessionId"] // NOTE: Key corrected to "sessionId"
    
    if okPathID {
        // Handle pathID which might be a float64 (from a number like 17) or a string
        if idStr, ok := pathIDVal.(string); ok {
            pathID = idStr
        } else if idFloat, ok := pathIDVal.(float64); ok {
            // Convert float64 number (like 17.0) to an integer string "17"
            pathID = fmt.Sprintf("%.0f", idFloat)
        }
    }
    
    // Set explicit values if they exist
    if okToken && authToken != "" {
        tc.SessionAuthToken = authToken
    }
    
    if pathID != "" { // Check pathID after type conversion
        tc.SessionPathID = pathID
    }
    
    // Fallback logic: If only one was found, use it for the other.
    if tc.SessionAuthToken != "" && tc.SessionPathID == "" {
        tc.SessionPathID = tc.SessionAuthToken
    }
    if tc.SessionPathID != "" && tc.SessionAuthToken == "" {
        tc.SessionAuthToken = tc.SessionPathID
    }
    
    if tc.SessionAuthToken != "" { // Check if we successfully logged in by checking the Auth Token
        // log.Printf("[%s] [INFO]: Login successful. PathID acquired: %s, AuthToken acquired: %s.", 
        //     tc.Target, tc.SessionPathID, tc.SessionAuthToken)
        return nil
    }
	
	return fmt.Errorf("login successful, but could not find session token in API response fields 'token' or 'sessionId'")
}

// Logout terminates the active session.
func (tc *TargetCollector) Logout() error {
	if tc.SessionPathID == "" {
		return nil // Nothing to do if not logged in
	}
    
	req := tc.findSessionRequest("logout")
	if req == nil {
		return fmt.Errorf("logout request configuration missing")
	}

	// The SessionPathID substitution will now occur in performRequest
	_, err := tc.performRequest(req)
	if err != nil {
        // Log the error but don't return it as critical—it's cleanup.
		log.Printf("[%s] [WARN]: Logout request failed (token may be already expired): %v", tc.Target, err)
        return nil
	}

	// Clear session IDs
	tc.SessionAuthToken = "" 
    tc.SessionPathID = "" 
    return nil
}


// --- TargetCollector implements the prometheus.Collector interface ---

func (tc *TargetCollector) Describe(ch chan<- *prometheus.Desc) {
	// Left simple as the metrics are dynamic
}

func (tc *TargetCollector) Collect(ch chan<- prometheus.Metric) {
	targetKey := tc.Target // Use the target URL as the cache key
	
	// --- Caching Check (Read Lock) ---
	tc.Exporter.CacheMutex.RLock()
	lastTime, exists := tc.Exporter.LastScrape[targetKey]
	cachedMetrics, metricsExist := tc.Exporter.ScrapeData[targetKey]
	tc.Exporter.CacheMutex.RUnlock()
	
	// Check if the cache is valid (exists and is within the interval)
	if exists && metricsExist && time.Since(lastTime) < tc.Exporter.CacheInterval {
		log.Printf("[%s] [INFO]: Serving from cache (last scrape: %v)", tc.Target, lastTime.Format(time.Stamp))
		for _, m := range cachedMetrics {
			ch <- m
		}
		return // Serve from cache and exit
	}

	// Cache miss or expired: proceed with actual scrape
	metrics, err := tc.scrapeTarget()
	if err != nil {
		log.Printf("[%s] [ERROR]: error during scrape. Serving stale data if available. Error: %v", tc.Target, err)
		
		// On error, fall back to serving stale data from cache (if any)
		if metricsExist {
			for _, m := range cachedMetrics {
				ch <- m
			}
		}
		return
	}

	// --- Update Cache (Write Lock) ---
	tc.Exporter.CacheMutex.Lock()
	tc.Exporter.ScrapeData[targetKey] = metrics
	tc.Exporter.LastScrape[targetKey] = time.Now()
	tc.Exporter.CacheMutex.Unlock()
	
	// Send newly scraped metrics
	for _, m := range metrics {
		ch <- m
	}
}

// --- Core Data Fetching and Authentication (MODIFIED) ---

// fetchData makes the actual HTTP call to the target device, using the active SessionID or Basic Auth.
func (tc *TargetCollector) fetchData(endpoint string) (string, error) {
	// Check if the target already has a protocol (http:// or https://)
	target := tc.Target
	if !(len(target) >= 7 && (target[:7] == "http://" || target[:8] == "https://")) {
		target = "http://" + target 
	}

	fullURL := target + endpoint 

	req, err := http.NewRequest("GET", fullURL, nil)
	if err != nil {
		return "", err
	}
	
	// AUTHENTICATION LOGIC: Use SessionAuthToken if available, otherwise fallback to Basic Auth
	if tc.SessionAuthToken != "" {
        // Use "Session" prefix based on user's data curl command.
		req.Header.Set("Authorization", fmt.Sprintf("Session %s", tc.SessionAuthToken)) 
        // log.Printf("[%s] [DEBUG]: Using session token (Session prefix) for request to %s", tc.Target, endpoint)
	} else {
        // Fallback to Basic Auth (Original logic)
        req.SetBasicAuth(tc.Username, tc.Password) 
        // log.Printf("[%s] [DEBUG]: Using Basic Auth for request to %s", tc.Target, endpoint)
    }

	resp, err := tc.Client.Do(req) 
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP status error from %s: %s", fullURL, resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return string(body), nil
}

// scrapeTarget iterates through all configured components and collects metrics.
func (tc *TargetCollector) scrapeTarget() ([]prometheus.Metric, error) {
	var allMetrics []prometheus.Metric
	
	// Iterate directly over the components loaded in the AppConfig
	for _, component := range tc.Exporter.Config.Components { 
		// Fetch data for the component's endpoint
		jsonBody, err := tc.fetchData(component.Endpoint)
		if err != nil {
			log.Printf("[%s] [ERROR]: fetching component %s from %s: %v", 
				tc.Target, component.Name, component.Endpoint, err)
			
			// Log context based on the authentication method used
			if tc.SessionAuthToken == "" {
			    log.Printf("[%s] [ERROR]: Basic Auth credentials used (u/p: %s/%s)",
			         tc.Target, tc.Username, tc.Password)
			} else {
			    log.Printf("[%s] [ERROR]: Session token used for API call. Error likely due to expired session or API issue.", tc.Target)
			}
			
			continue
		}

		// Process all metrics defined within this component
		componentMetrics := tc.processComponentMetrics(jsonBody, component)
		allMetrics = append(allMetrics, componentMetrics...)
	}

	// Model discovery is skipped in this simplified version.
	return allMetrics, nil
}

// processComponentMetrics handles value and label extraction, including array handling.
func (tc *TargetCollector) processComponentMetrics(jsonBody string, comp config.ComponentDefinition) []prometheus.Metric {
	var metrics []prometheus.Metric
	jsonString := jsonBody 
	
	// Helper function to extract all dynamic label values for a metric instance
	extractLabelValues := func(body string, defs []config.LabelDefinition, index int) []string {
		values := make([]string, 0, len(defs))
		values = append(values, tc.Target) // Always start with 'target' label
		
		for _, labelDef := range defs {
			labelValueResult := gjson.Get(body, labelDef.JsonPath)
			
			// Check for array and use index, or get scalar value
			if labelValueResult.IsArray() && labelValueResult.Get(fmt.Sprintf("%d", index)).Exists() {
				values = append(values, labelValueResult.Get(fmt.Sprintf("%d", index)).String())
			} else if labelValueResult.Exists() && !labelValueResult.IsArray() {
				 values = append(values, labelValueResult.String())
			} else {
				values = append(values, "")
			}
		}
		return values
	}

	for _, metricDef := range comp.Metrics {
		metricResult := gjson.Get(jsonString, metricDef.JsonPath)
		metricType := prometheus.GaugeValue
		if metricDef.Type == "counter" {
			metricType = prometheus.CounterValue
		}

		// Define the set of constant label names
		labelNames := make([]string, 0, 1+len(metricDef.Labels))
		labelNames = append(labelNames, "target") 
		for _, labelDef := range metricDef.Labels {
			labelNames = append(labelNames, labelDef.Name)
		}
		
		desc := prometheus.NewDesc(
			prometheus.BuildFQName("san", "", metricDef.Name),
			metricDef.Help,
			labelNames,
			nil,
		)
		
		metricValue := metricResult.Float()
		// --- A) Handle Array/Multi-Value Metrics ---
		if metricResult.IsArray() {
			metricResult.ForEach(func(i, value gjson.Result) bool {
				
				if value.Type != gjson.Number {
					metricValue = 0
				}

				labelValues := extractLabelValues(jsonString, metricDef.Labels, int(i.Int()))

				m, err := prometheus.NewConstMetric(desc, metricType, value.Float(), labelValues...)
				if err == nil {
					metrics = append(metrics, m)
				} else {
					log.Printf("[%s] [ERROR]: creating array metric %s: %v", tc.Target, metricDef.Name, err)
				}
				return true
			})
		} else if metricResult.Exists() {
			// --- B) Handle Single-Value Metrics ---
			
			if metricResult.Type != gjson.Number {
				metricValue = 0
			}

			labelValues := extractLabelValues(jsonString, metricDef.Labels, 0)

			m, err := prometheus.NewConstMetric(desc, metricType, metricValue, labelValues...)
			if err == nil {
				metrics = append(metrics, m)
			} else {
				log.Printf("[%s] [ERROR]: creating single metric %s: %v", tc.Target, metricDef.Name, err)
			}
		} else {
			log.Printf("[%s] [WARN]: Metric %s value not found at path: %s", tc.Target, metricDef.Name, metricDef.JsonPath)
		}
	}
	return metrics
}
