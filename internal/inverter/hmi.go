package inverter

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

// HMIDataPoint is a single data sample sent to the web UI via WebSocket.
type HMIDataPoint struct {
	Time    string  `json:"time"`    // formatted timestamp
	SimTime string  `json:"simTime"` // simulation time
	P       float64 `json:"p"`       // active power (W)
	Q       float64 `json:"q"`       // reactive power (VAr)
	PF      float64 `json:"pf"`      // power factor
	V       float64 `json:"v"`       // voltage (p.u.)
	F       float64 `json:"f"`       // frequency (Hz)
	Mode    string  `json:"mode"`    // control mode name
	Connected bool  `json:"connected"`
	Irradiance float64 `json:"irradiance"` // W/m²
}

// HMI serves the real-time web dashboard with Apache ECharts.
type HMI struct {
	mu      sync.RWMutex
	clients map[chan []byte]struct{}
}

// NewHMI creates a new HMI server.
func NewHMI() *HMI {
	return &HMI{
		clients: make(map[chan []byte]struct{}),
	}
}

// Broadcast sends a data point to all connected WebSocket clients.
func (h *HMI) Broadcast(dp HMIDataPoint) {
	data, err := json.Marshal(dp)
	if err != nil {
		return
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	for ch := range h.clients {
		select {
		case ch <- data:
		default:
			// client too slow, drop
		}
	}
}

// Handler returns the HTTP handler for the HMI.
func (h *HMI) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", h.handleDashboard)
	mux.HandleFunc("/ws", h.handleWebSocket)
	return mux
}

func (h *HMI) handleDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprint(w, dashboardHTML)
}

func (h *HMI) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Simple SSE (Server-Sent Events) fallback since we're not importing
	// a WebSocket library. SSE works for one-way data push and needs no deps.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	ch := make(chan []byte, 50)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.clients, ch)
		h.mu.Unlock()
	}()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case data := <-ch:
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>IEEE 1547 Inverter Simulator</title>
<script src="https://cdn.jsdelivr.net/npm/apache-echarts@5/dist/echarts.min.js"></script>
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif; background: #1a1a2e; color: #e0e0e0; }
  .header { padding: 16px 24px; background: #16213e; border-bottom: 2px solid #0f3460; display: flex; align-items: center; justify-content: space-between; }
  .header h1 { font-size: 18px; color: #e94560; }
  .status { display: flex; gap: 24px; }
  .status-item { text-align: center; }
  .status-item .label { font-size: 11px; color: #888; text-transform: uppercase; }
  .status-item .value { font-size: 20px; font-weight: bold; font-family: 'Courier New', monospace; }
  .connected { color: #4ecca3; }
  .disconnected { color: #e94560; }
  .charts { display: grid; grid-template-columns: 1fr 1fr; gap: 8px; padding: 8px; height: calc(100vh - 70px); }
  .chart-container { background: #16213e; border-radius: 8px; border: 1px solid #0f3460; }
</style>
</head>
<body>
<div class="header">
  <h1>IEEE 1547 Inverter Simulator — Real-Time Dashboard</h1>
  <div class="status">
    <div class="status-item"><div class="label">Mode</div><div class="value" id="mode">—</div></div>
    <div class="status-item"><div class="label">P (W)</div><div class="value" id="power">—</div></div>
    <div class="status-item"><div class="label">Q (VAr)</div><div class="value" id="reactive">—</div></div>
    <div class="status-item"><div class="label">PF</div><div class="value" id="pf">—</div></div>
    <div class="status-item"><div class="label">Status</div><div class="value" id="connStatus">—</div></div>
  </div>
</div>
<div class="charts">
  <div class="chart-container" id="chartPower"></div>
  <div class="chart-container" id="chartVoltage"></div>
  <div class="chart-container" id="chartReactive"></div>
  <div class="chart-container" id="chartFrequency"></div>
</div>
<script>
const MAX_POINTS = 300;
const powerData = [], voltageData = [], reactiveData = [], freqData = [];

function makeChart(el, title, yLabel, color) {
  const chart = echarts.init(document.getElementById(el), 'dark');
  chart.setOption({
    backgroundColor: 'transparent',
    title: { text: title, left: 'center', textStyle: { color: '#ccc', fontSize: 14 } },
    tooltip: { trigger: 'axis' },
    xAxis: { type: 'category', data: [], axisLabel: { color: '#888', fontSize: 10 } },
    yAxis: { type: 'value', name: yLabel, nameTextStyle: { color: '#888' }, axisLabel: { color: '#888' }, splitLine: { lineStyle: { color: '#333' } } },
    series: [{ type: 'line', smooth: true, symbol: 'none', lineStyle: { width: 2, color: color }, areaStyle: { color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [{offset: 0, color: color + '40'}, {offset: 1, color: 'transparent'}]) }, data: [] }],
    grid: { left: 60, right: 20, top: 40, bottom: 30 },
    animation: false
  });
  window.addEventListener('resize', () => chart.resize());
  return chart;
}

const chartP = makeChart('chartPower', 'Active Power', 'Watts', '#4ecca3');
const chartV = makeChart('chartVoltage', 'Voltage', 'p.u.', '#e94560');
const chartQ = makeChart('chartReactive', 'Reactive Power', 'VAr', '#ffd700');
const chartF = makeChart('chartFrequency', 'Frequency', 'Hz', '#00b4d8');

function updateChart(chart, dataArr, time, value) {
  dataArr.push({ time, value });
  if (dataArr.length > MAX_POINTS) dataArr.shift();
  chart.setOption({
    xAxis: { data: dataArr.map(d => d.time) },
    series: [{ data: dataArr.map(d => d.value) }]
  });
}

const evtSource = new EventSource('/ws');
evtSource.onmessage = function(event) {
  const d = JSON.parse(event.data);
  const t = d.simTime || d.time;

  document.getElementById('mode').textContent = d.mode;
  document.getElementById('power').textContent = Math.round(d.p).toLocaleString();
  document.getElementById('reactive').textContent = Math.round(d.q).toLocaleString();
  document.getElementById('pf').textContent = d.pf.toFixed(3);

  const statusEl = document.getElementById('connStatus');
  statusEl.textContent = d.connected ? 'ONLINE' : 'OFFLINE';
  statusEl.className = d.connected ? 'value connected' : 'value disconnected';

  updateChart(chartP, powerData, t, d.p);
  updateChart(chartV, voltageData, t, d.v);
  updateChart(chartQ, reactiveData, t, d.q);
  updateChart(chartF, freqData, t, d.f);
};
evtSource.onerror = function() { console.log('SSE connection lost, retrying...'); };
</script>
</body>
</html>`
