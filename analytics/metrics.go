// Copyright 2019 Kuei-chun Chen. All rights reserved.

package analytics

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// Metrics stores metrics from FTDC data
type Metrics struct {
	sync.RWMutex
	summaryFTDC FTDCStats
	detailFTDC  FTDCStats
	filenames   []string
}

// NewMetrics returns &Metrics
func NewMetrics(filenames []string) *Metrics {
	metrics := &Metrics{filenames: filenames}
	diag := NewDiagnosticData(300)
	if err := diag.DecodeDiagnosticData(filenames); err != nil { // get summary
		log.Fatal(err)
	}
	metrics.SetFTDCSummaryStats(diag)
	go func(m *Metrics, diag *DiagnosticData, filenames []string) {
		diag = NewDiagnosticData(1)
		diag.DecodeDiagnosticData(filenames)
		m.SetFTDCDetailStats(diag)
	}(metrics, diag, filenames)
	return metrics
}

// Handler handle HTTP requests
func (m *Metrics) Handler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path[1:] == "grafana/" {
		fmt.Fprintf(w, "ok\n")
	} else if r.URL.Path[1:] == "grafana/query" {
		m.query(w, r)
	} else if r.URL.Path[1:] == "grafana/search" {
		m.search(w, r)
	} else if r.URL.Path[1:] == "grafana/dir" {
		m.readDirectory(w, r)
	}
}

// SetFTDCSummaryStats populate FTDC summary, 5 minutes span
func (m *Metrics) SetFTDCSummaryStats(diag *DiagnosticData) {
	m.RLock()
	defer m.RUnlock()
	setFTDCStats(diag, &m.summaryFTDC)
}

// SetFTDCDetailStats populate FTDC details, 1 second span
func (m *Metrics) SetFTDCDetailStats(diag *DiagnosticData) {
	m.RLock()
	defer m.RUnlock()
	setFTDCStats(diag, &m.detailFTDC)
}

type directoryReq struct {
	Dir  string `json:"dir"`
	Span int    `json:"span"`
}

func (m *Metrics) readDirectory(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
	case http.MethodPost:
		var err error
		decoder := json.NewDecoder(r.Body)
		var dr directoryReq
		if err = decoder.Decode(&dr); err != nil {
			json.NewEncoder(w).Encode(bson.M{"ok": 0, "err": err.Error()})
			return
		}
		var filenames = []string{dr.Dir}
		diag := NewDiagnosticData(300) // summary
		if err = diag.DecodeDiagnosticData(filenames); err != nil {
			json.NewEncoder(w).Encode(bson.M{"ok": 0, "err": err.Error()})
			return
		}
		m.SetFTDCSummaryStats(diag)

		diag = NewDiagnosticData(1)
		if err = diag.DecodeDiagnosticData(filenames); err != nil {
			json.NewEncoder(w).Encode(bson.M{"ok": 0, "err": err.Error()})
			return
		}
		m.SetFTDCDetailStats(diag)
		json.NewEncoder(w).Encode(bson.M{"ok": 1, "dir": dr.Dir})
	default:
		http.Error(w, "bad method; supported OPTIONS, POST", http.StatusBadRequest)
		return
	}
}

func (m *Metrics) search(w http.ResponseWriter, r *http.Request) {
	var list []string

	for _, doc := range m.detailFTDC.timeSeriesData {
		list = append(list, doc.Target)
	}

	list = append(list, "host_info")
	json.NewEncoder(w).Encode(list)
}

func (m *Metrics) query(w http.ResponseWriter, r *http.Request) {
	decoder := json.NewDecoder(r.Body)
	var qr QueryRequest
	if err := decoder.Decode(&qr); err != nil {
		return
	}

	ftdc := m.summaryFTDC
	if qr.Range.To.Sub(qr.Range.From).Hours() < 24 {
		ftdc = m.detailFTDC
	}
	var tsData []interface{}
	for _, target := range qr.Targets {
		if target.Type == "timeserie" {
			if target.Target == "replication_lags" { // replaced with actual hostname
				for k, v := range ftdc.replicationLags {
					data := v
					data.Target = k
					tsData = append(tsData, filterTimeSeriesData(data, qr.Range.From, qr.Range.To))
				}
			} else if target.Target == "disks_utils" {
				for k, v := range ftdc.diskStats {
					data := v.utilization
					data.Target = k
					tsData = append(tsData, filterTimeSeriesData(data, qr.Range.From, qr.Range.To))
				}
			} else if target.Target == "disks_iops" {
				for k, v := range ftdc.diskStats {
					data := v.iops
					data.Target = k
					tsData = append(tsData, filterTimeSeriesData(data, qr.Range.From, qr.Range.To))
				}
			} else {
				tsData = append(tsData, filterTimeSeriesData(ftdc.timeSeriesData[target.Target], qr.Range.From, qr.Range.To))
			}
		} else if target.Type == "table" {
			if target.Target == "host_info" {
				headerList := []bson.M{}
				headerList = append(headerList, bson.M{"text": "Info", "type": "string"})
				headerList = append(headerList, bson.M{"text": "Value", "type": "string"})
				var si ServerInfoDoc
				b, _ := json.Marshal(ftdc.serverInfo)
				if err := json.Unmarshal(b, &si); err != nil {
					rowList := [][]string{[]string{"Error", err.Error()}}
					doc1 := bson.M{"columns": headerList, "type": "table", "rows": rowList}
					tsData = append(tsData, doc1)
					continue
				}
				rowList := [][]string{}

				rowList = append(rowList, []string{"CPU", strconv.Itoa(si.HostInfo.System.NumCores) + " cores (" + si.HostInfo.System.CPUArch + ")"})
				rowList = append(rowList, []string{"Hostname", si.HostInfo.System.Hostname})
				rowList = append(rowList, []string{"Memory (MB)", strconv.Itoa(si.HostInfo.System.MemSizeMB)})
				rowList = append(rowList, []string{"MongoDB Version", si.BuildInfo.Version})
				rowList = append(rowList, []string{"OS", si.HostInfo.OS.Name})
				rowList = append(rowList, []string{"OS Type", si.HostInfo.OS.Type + " (" + si.HostInfo.OS.Version + ")"})
				doc1 := bson.M{"columns": headerList, "type": "table", "rows": rowList}
				tsData = append(tsData, doc1)
			}
		}
	}
	json.NewEncoder(w).Encode(tsData)
}

func filterTimeSeriesData(tsData TimeSeriesDoc, from time.Time, to time.Time) TimeSeriesDoc {
	var data = TimeSeriesDoc{DataPoints: [][]float64{}}
	data.Target = tsData.Target
	for _, v := range tsData.DataPoints {
		tm := time.Unix(0, int64(v[1])*int64(time.Millisecond))
		if tm.After(to) || tm.Before(from) {
			continue
		}
		data.DataPoints = append(data.DataPoints, v)
	}

	max := 1000 // max data points, 5 minutes * 1000 = 84 hours
	if len(data.DataPoints) > max {
		frac := len(data.DataPoints) / max
		var datax = TimeSeriesDoc{DataPoints: [][]float64{}}
		datax.Target = tsData.Target
		count := 0
		sum := float64(0)
		last := len(data.DataPoints) - 1
		for i, v := range data.DataPoints {
			if v[0] > 0 || count == 0 { // has value or the first value
				sum += v[0]
				count++
			}
			if i%frac != 0 && i != last {
				continue
			}
			v[0] = math.Round(sum / float64(count))
			datax.DataPoints = append(datax.DataPoints, v)
			count = 0
			sum = 0
		}
		return datax
	}
	return data
}
