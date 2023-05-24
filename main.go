package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jamessanford/remote-tsdb-clickhouse/internal/clickhouse"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

var (
	samplesWrittenTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "samples_written_total",
			Help: "number of samples written into clickhouse",
		})
	writeRequestsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "write_requests_total",
			Help: "number of hits to write endpoint",
		})
	writeErrorsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "write_errors_total",
			Help: "number of errors generated by write endpoint",
		})
	readRequestsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "read_requests_total",
			Help: "number of hits to read endpoint",
		})
	readErrorsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "read_errors_total",
			Help: "number of errors generated by read endpoint",
		})
)

func init() {
	prometheus.MustRegister(samplesWrittenTotal)
	prometheus.MustRegister(writeRequestsTotal)
	prometheus.MustRegister(writeErrorsTotal)
	prometheus.MustRegister(readRequestsTotal)
	prometheus.MustRegister(readErrorsTotal)
}

func read(ch *clickhouse.ClickHouseAdapter, w http.ResponseWriter, r *http.Request) error {
	req, err := DecodeReadRequest(r.Body)
	if err != nil {
		return fmt.Errorf("DecodeReadRequest: %w", err)
	}

	res, err := ch.ReadRequest(r.Context(), req)
	if err != nil {
		return fmt.Errorf("ReadRequest: %w", err)
	}

	w.Header().Set("Content-Type", "application/x-protobuf")
	w.Header().Set("Content-Encoding", "snappy")

	if err := EncodeReadResponse(res, w); err != nil {
		return fmt.Errorf("EncodeReadResponse: %w", err)
	}

	return nil
}

func main() {
	var httpAddr string
	var clickAddr, database, username, password, table string
	var readRequestIgnoreLabel string
	flag.StringVar(&httpAddr, "http", "9131", "listen on this [address:]port")
	flag.StringVar(&clickAddr, "db", "127.0.0.1:9000", "ClickHouse DB at this address:port")
	flag.StringVar(&database, "db.database", "default", "ClickHouse database")
	flag.StringVar(&username, "db.username", "default", "ClickHouse username")
	flag.StringVar(&password, "db.password", "", "ClickHouse password")
	flag.StringVar(&table, "table", "metrics.samples", "write to this database.tablename")
	flag.StringVar(&readRequestIgnoreLabel, "read.ignore-label", "remote=clickhouse", "ignore this label in read requests")
	flag.Parse()

	if !strings.Contains(httpAddr, ":") {
		httpAddr = ":" + httpAddr
	}

	logger, err := zap.NewProduction()
	if err != nil {
		panic(err)
	}

	ch, err := clickhouse.NewClickHouseAdapter(clickAddr, database, username, password, table)
	if err != nil {
		logger.Fatal("NewClickHouseAdapter", zap.Error(err))
	}

	if readRequestIgnoreLabel != "" {
		ch.IgnoreLabelInReadRequests(readRequestIgnoreLabel)
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "remote-tsdb-clickhouse")
		r.Body.Close()
	})

	http.Handle("/metrics", promhttp.Handler())

	http.HandleFunc("/write", func(w http.ResponseWriter, r *http.Request) {
		writeRequestsTotal.Inc()
		defer r.Body.Close()
		req, err := DecodeWriteRequest(r.Body)
		if err != nil {
			writeErrorsTotal.Inc()
			logger.Error("DecodeWriteRequest", zap.Error(err))
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if count, err := ch.WriteRequest(r.Context(), req); err != nil {
			writeErrorsTotal.Inc()
			logger.Error("WriteRequest", zap.Error(err))
			http.Error(w, err.Error(), http.StatusInternalServerError)
		} else if count > 0 {
			samplesWrittenTotal.Add(float64(count))
		}
	})

	http.HandleFunc("/read", func(w http.ResponseWriter, r *http.Request) {
		readRequestsTotal.Inc()
		defer r.Body.Close()
		if err := read(ch, w, r); err != nil && !errors.Is(err, context.Canceled) {
			readErrorsTotal.Inc()
			logger.Error("ReadRequest", zap.Error(err))
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	logger.Info(
		"listening",
		zap.String("listen", httpAddr),
		zap.String("db", clickAddr),
		zap.String("table", table),
	)

	if err := http.ListenAndServe(httpAddr, nil); err != nil {
		logger.Fatal("ListenAndServe", zap.Error(err))
	}
}
