package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

var GetMuniArrivalsDefinition = ToolDefinition{
	Name:        "get_muni_arrivals",
	Description: "Get real-time Muni arrival predictions for a stop. When called without a direction, returns the available directions at that stop so you can ask the user which way they're going. When called with a direction, returns the next 5 trains/buses in that direction.",
	InputSchema: GenerateSchema[GetMuniArrivalsInput](),
	Function:    GetMuniArrivals,
}

type GetMuniArrivalsInput struct {
	Location  string `json:"location" jsonschema_description:"A Muni stop name (e.g. 'Church Station', 'Castro & Market') or a street address in San Francisco (e.g. '123 Market St')."`
	Direction string `json:"direction,omitempty" jsonschema_description:"Optional. The direction of travel: 'IB' (inbound/downtown) or 'OB' (outbound). Leave empty to see available directions first."`
}

func GetMuniArrivals(input json.RawMessage) (string, error) {
	var params GetMuniArrivalsInput
	if err := json.Unmarshal(input, &params); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}

	allStops, err := getMuniStops()
	if err != nil {
		return "", fmt.Errorf("failed to get stops: %w", err)
	}

	// Find all matching stops (e.g. both "Metro Church Station/Downtown" and "/Outbound")
	matchedStops := findStopsByName(params.Location, allStops)
	if len(matchedStops) == 0 {
		lat, lon, err := geocodeAddress(params.Location)
		if err != nil {
			return "", fmt.Errorf("could not find stop or geocode location: %w", err)
		}
		matchedStops = []muniStop{findNearestStop(lat, lon, allStops)}
	}

	// Query all matched stops and merge departures
	var allDepartures []departure
	var stopNames []string
	for _, stop := range matchedStops {
		fmt.Printf("\033[90m  → querying stop: %s (ID: %s)\033[0m\n", stop.Name, stop.ID)
		deps, err := getStopMonitoring(stop.ID)
		if err != nil {
			fmt.Printf("\033[91m  → error for stop %s: %s\033[0m\n", stop.ID, err)
			continue
		}
		allDepartures = append(allDepartures, deps...)
		stopNames = append(stopNames, stop.Name)
	}

	sort.Slice(allDepartures, func(i, j int) bool {
		return allDepartures[i].minutes < allDepartures[j].minutes
	})

	dir := normalizeDirection(params.Direction)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Station: %s\n\n", strings.Join(stopNames, " / ")))

	if dir == "" {
		sb.WriteString(formatDirectionSummary(allDepartures))
	} else {
		sb.WriteString(formatFilteredDepartures(allDepartures, dir))
	}

	return sb.String(), nil
}

func normalizeDirection(d string) string {
	d = strings.ToUpper(strings.TrimSpace(d))
	switch {
	case d == "":
		return ""
	case d == "IB" || strings.Contains(d, "INBOUND") || strings.Contains(d, "DOWNTOWN"):
		return "IB"
	case d == "OB" || strings.Contains(d, "OUTBOUND"):
		return "OB"
	default:
		return d
	}
}

func formatDirectionSummary(departures []departure) string {
	dirs := map[string][]departure{}
	for _, d := range departures {
		dirs[d.directionRef] = append(dirs[d.directionRef], d)
	}

	if len(dirs) == 0 {
		return "No upcoming departures at this time."
	}

	var sb strings.Builder
	sb.WriteString("Available directions:\n\n")

	for dir, deps := range dirs {
		label := dir
		if dir == "IB" {
			label = "IB (Inbound/Downtown)"
		} else if dir == "OB" {
			label = "OB (Outbound)"
		}

		destinations := map[string]bool{}
		lines := map[string]bool{}
		for _, d := range deps {
			destinations[d.destination] = true
			lines[d.routeID] = true
		}

		destList := []string{}
		for dest := range destinations {
			destList = append(destList, dest)
		}
		sort.Strings(destList)

		lineList := []string{}
		for line := range lines {
			lineList = append(lineList, line)
		}
		sort.Strings(lineList)

		sb.WriteString(fmt.Sprintf("  %s\n", label))
		sb.WriteString(fmt.Sprintf("    Lines: %s\n", strings.Join(lineList, ", ")))
		sb.WriteString(fmt.Sprintf("    Toward: %s\n\n", strings.Join(destList, "; ")))
	}

	sb.WriteString("Based on the user's destination, pick the correct direction and immediately call this tool again with the direction parameter set to 'IB' or 'OB'. If you cannot figure out the direction, ask the user for clarification.")
	return sb.String()
}

func formatFilteredDepartures(departures []departure, dir string) string {
	var filtered []departure
	for _, d := range departures {
		if d.directionRef == dir {
			filtered = append(filtered, d)
		}
	}

	if len(filtered) > 15 {
		filtered = filtered[:15]
	}

	if len(filtered) == 0 {
		return fmt.Sprintf("No upcoming departures in direction %s.", dir)
	}

	var sb strings.Builder
	label := dir
	if dir == "IB" {
		label = "Inbound/Downtown"
	} else if dir == "OB" {
		label = "Outbound"
	}
	sb.WriteString(fmt.Sprintf("Next departures (%s):\n", label))
	for _, d := range filtered {
		rt := ""
		if d.isRealTime {
			rt = "*"
		}
		sb.WriteString(fmt.Sprintf("  [%s] %s → %s — %d min%s\n", d.routeID, d.line, d.destination, d.minutes, rt))
	}
	sb.WriteString("\n* = real-time estimate")
	return sb.String()
}

func findStopsByName(query string, stops []muniStop) []muniStop {
	query = strings.ToLower(query)

	// Exact match
	var exact []muniStop
	for _, s := range stops {
		if strings.ToLower(s.Name) == query {
			exact = append(exact, s)
		}
	}
	if len(exact) > 0 {
		return exact
	}

	// Substring match — find all stops whose name contains the query
	var matches []muniStop
	for _, s := range stops {
		lower := strings.ToLower(s.Name)
		if strings.Contains(lower, query) {
			matches = append(matches, s)
		}
	}

	// If too many matches, prefer "Metro" stops (underground stations)
	if len(matches) > 5 {
		var metro []muniStop
		for _, s := range matches {
			if strings.HasPrefix(strings.ToLower(s.Name), "metro") {
				metro = append(metro, s)
			}
		}
		if len(metro) > 0 {
			return metro
		}
	}

	return matches
}

// --- Geocoding (OpenStreetMap Nominatim) ---

func geocodeAddress(address string) (float64, float64, error) {
	if !strings.Contains(strings.ToLower(address), "san francisco") {
		address += ", San Francisco, CA"
	}

	u := fmt.Sprintf("https://nominatim.openstreetmap.org/search?q=%s&format=json&limit=1",
		url.QueryEscape(address))

	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("User-Agent", "SFMuniAgent/1.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()

	var results []struct {
		Lat string `json:"lat"`
		Lon string `json:"lon"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return 0, 0, err
	}
	if len(results) == 0 {
		return 0, 0, fmt.Errorf("no geocoding results for: %s", address)
	}

	var lat, lon float64
	fmt.Sscanf(results[0].Lat, "%f", &lat)
	fmt.Sscanf(results[0].Lon, "%f", &lon)
	return lat, lon, nil
}

// --- Muni Stops (cached from 511 API) ---

type muniStop struct {
	ID       string
	Name     string
	Lat      float64
	Lon      float64
	Distance float64
}

var (
	cachedStops    []muniStop
	cachedStopsErr error
	stopsOnce      sync.Once
)

func getMuniStops() ([]muniStop, error) {
	stopsOnce.Do(func() {
		apiKey := os.Getenv("MUNI_API_KEY")
		if apiKey == "" {
			cachedStopsErr = fmt.Errorf("MUNI_API_KEY environment variable not set")
			return
		}

		u := fmt.Sprintf("https://api.511.org/transit/stops?api_key=%s&operator_id=SF&format=json", apiKey)
		body, status, err := fetchJSON(u)
		if err != nil {
			cachedStopsErr = err
			return
		}
		if status != 200 {
			cachedStopsErr = fmt.Errorf("Stops API returned %d: %s", status, truncateStr(string(body), 200))
			return
		}

		var parsed struct {
			Contents struct {
				DataObjects struct {
					ScheduledStopPoint []struct {
						ID       string `json:"id"`
						Name     string `json:"Name"`
						Location struct {
							Latitude  string `json:"Latitude"`
							Longitude string `json:"Longitude"`
						} `json:"Location"`
					} `json:"ScheduledStopPoint"`
				} `json:"dataObjects"`
			} `json:"Contents"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			cachedStopsErr = fmt.Errorf("failed to parse stops (first 500 chars: %s): %w",
				truncateStr(string(body), 500), err)
			return
		}

		for _, sp := range parsed.Contents.DataObjects.ScheduledStopPoint {
			var lat, lon float64
			fmt.Sscanf(sp.Location.Latitude, "%f", &lat)
			fmt.Sscanf(sp.Location.Longitude, "%f", &lon)
			if lat != 0 && lon != 0 {
				cachedStops = append(cachedStops, muniStop{
					ID:   sp.ID,
					Name: sp.Name,
					Lat:  lat,
					Lon:  lon,
				})
			}
		}

		fmt.Printf("\033[90m  → loaded %d stops from 511 API\033[0m\n", len(cachedStops))
	})
	return cachedStops, cachedStopsErr
}

// --- Distance ---

func haversine(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadius = 6371000
	dLat := (lat2 - lat1) * math.Pi / 180
	dLon := (lon2 - lon1) * math.Pi / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*math.Pi/180)*math.Cos(lat2*math.Pi/180)*
			math.Sin(dLon/2)*math.Sin(dLon/2)
	return earthRadius * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

func findNearestStop(lat, lon float64, stops []muniStop) muniStop {
	var nearest muniStop
	minDist := math.MaxFloat64
	for _, s := range stops {
		d := haversine(lat, lon, s.Lat, s.Lon)
		if d < minDist {
			minDist = d
			nearest = s
			nearest.Distance = d
		}
	}
	return nearest
}

// --- Real-time Stop Monitoring (matches tessro/muni patterns) ---

type departure struct {
	routeID      string
	line         string
	destination  string
	directionRef string
	minutes      int
	isRealTime   bool
}

func getStopMonitoring(stopCode string) ([]departure, error) {
	apiKey := os.Getenv("MUNI_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("MUNI_API_KEY environment variable not set")
	}

	params := url.Values{}
	params.Set("api_key", apiKey)
	params.Set("agency", "SF")
	params.Set("stopCode", stopCode)
	params.Set("format", "json")
	u := "https://api.511.org/transit/StopMonitoring?" + params.Encode()

	body, status, err := fetchJSON(u)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("StopMonitoring API returned %d: %s", status, truncateStr(string(body), 200))
	}

	var parsed struct {
		ServiceDelivery struct {
			StopMonitoringDelivery struct {
				MonitoredStopVisit []struct {
					MonitoredVehicleJourney struct {
						LineRef           string `json:"LineRef"`
						DirectionRef      string `json:"DirectionRef"`
						PublishedLineName string `json:"PublishedLineName"`
						DestinationName   string `json:"DestinationName"`
						Monitored         bool   `json:"Monitored"`
						MonitoredCall     struct {
							ExpectedArrivalTime   string `json:"ExpectedArrivalTime"`
							AimedArrivalTime      string `json:"AimedArrivalTime"`
							ExpectedDepartureTime string `json:"ExpectedDepartureTime"`
							AimedDepartureTime    string `json:"AimedDepartureTime"`
						} `json:"MonitoredCall"`
					} `json:"MonitoredVehicleJourney"`
				} `json:"MonitoredStopVisit"`
			} `json:"StopMonitoringDelivery"`
		} `json:"ServiceDelivery"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("failed to parse response (first 500 chars: %s): %w",
			truncateStr(string(body), 500), err)
	}

	now := time.Now()
	var departures []departure
	for _, visit := range parsed.ServiceDelivery.StopMonitoringDelivery.MonitoredStopVisit {
		mvj := visit.MonitoredVehicleJourney
		call := mvj.MonitoredCall

		var departureTime time.Time
		var isRealTime bool

		// Prefer expected (real-time) over aimed (scheduled), departure over arrival
		if t, err := time.Parse(time.RFC3339, call.ExpectedDepartureTime); err == nil {
			departureTime = t
			isRealTime = mvj.Monitored
		} else if t, err := time.Parse(time.RFC3339, call.AimedDepartureTime); err == nil {
			departureTime = t
		} else if t, err := time.Parse(time.RFC3339, call.ExpectedArrivalTime); err == nil {
			departureTime = t
			isRealTime = mvj.Monitored
		} else if t, err := time.Parse(time.RFC3339, call.AimedArrivalTime); err == nil {
			departureTime = t
		}

		if departureTime.IsZero() {
			continue
		}

		mins := int(departureTime.Sub(now).Minutes())
		if mins < 0 {
			continue
		}

		lineName := mvj.PublishedLineName
		if lineName == "" {
			lineName = mvj.LineRef
		}

		departures = append(departures, departure{
			routeID:      mvj.LineRef,
			line:         lineName,
			destination:  mvj.DestinationName,
			directionRef: mvj.DirectionRef,
			minutes:      mins,
			isRealTime:   isRealTime,
		})
	}

	sort.Slice(departures, func(i, j int) bool {
		return departures[i].minutes < departures[j].minutes
	})

	return departures, nil
}

// --- Helpers ---

func fetchJSON(rawURL string) ([]byte, int, error) {
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, resp.StatusCode, err
		}
		defer gr.Close()
		reader = gr
	}

	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	body = bytes.TrimPrefix(body, []byte("\xef\xbb\xbf"))
	return body, resp.StatusCode, nil
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
