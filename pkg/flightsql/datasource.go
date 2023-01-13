package flightsql

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"

	"github.com/apache/arrow/go/v10/arrow/flight/flightsql"
	"github.com/go-chi/chi/v5"
	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/instancemgmt"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/grafana/grafana-plugin-sdk-go/backend/resource/httpadapter"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

var (
	_ backend.QueryDataHandler      = (*FlightSQLDatasource)(nil)
	_ backend.CheckHealthHandler    = (*FlightSQLDatasource)(nil)
	_ instancemgmt.InstanceDisposer = (*FlightSQLDatasource)(nil)
	_ backend.CallResourceHandler   = (*FlightSQLDatasource)(nil)
)

const mdBucketName = "bucket-name"

type config struct {
	Host     string `json:"host"`
	Database string `json:"database"`
	Token    string `json:"token"`
	Secure   bool   `json:"secure"`
}

// FlightSQLDatasource is a Grafana datasource plugin for Flight SQL.
type FlightSQLDatasource struct {
	database        string
	client          *flightsql.Client
	resourceHandler backend.CallResourceHandler
}

// NewDatasource creates a new datasource instance.
func NewDatasource(settings backend.DataSourceInstanceSettings) (instancemgmt.Instance, error) {
	var cfg config
	err := json.Unmarshal(settings.JSONData, &cfg)
	if err != nil {
		return nil, fmt.Errorf("config: %s", err)
	}

	dialOptions := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(insecureBearerToken{token: cfg.Token}),
		grpc.WithBlock(),
	}

	flightSQLSecure := cfg.Secure
	if flightSQLSecure {
		pool, err := x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("x509: %s", err)
		}

		dialOptions = []grpc.DialOption{
			grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(pool, "")),
			grpc.WithPerRPCCredentials(bearerToken{token: cfg.Token}),
			grpc.WithBlock(),
		}
	}

	client, err := flightsql.NewClient(cfg.Host, nil, nil, dialOptions...)

	if err != nil {
		return nil, fmt.Errorf("flightsql: %s", err)
	}

	ds := &FlightSQLDatasource{
		database: cfg.Database,
		client:   client,
	}
	r := chi.NewRouter()
	r.Use(recoverer)
	r.Get("/get-sql-info", ds.getSQLInfo)
	r.Get("/get-tables", ds.getTables)
	r.Get("/get-columns", ds.getColumns)
	ds.resourceHandler = httpadapter.New(r)

	return ds, nil
}

func recoverer(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				log.DefaultLogger.Error("Panic:", string(debug.Stack()))
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

// Dispose cleans up before we are reaped.
func (d *FlightSQLDatasource) Dispose() {
	if err := d.client.Close(); err != nil {
		log.DefaultLogger.Error(err.Error())
	}
}

func (d *FlightSQLDatasource) CallResource(ctx context.Context, req *backend.CallResourceRequest, sender backend.CallResourceResponseSender) error {
	return d.resourceHandler.CallResource(ctx, req, sender)
}

// QueryData fulfills query requests.
func (d *FlightSQLDatasource) QueryData(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
	response := backend.NewQueryDataResponse()

	for _, qreq := range req.Queries {
		var q queryRequest
		if err := json.Unmarshal(qreq.JSON, &q); err != nil {
			response.Responses[qreq.RefID] = backend.ErrDataResponse(backend.StatusBadRequest, fmt.Sprintf("unmarshal query request: %s", err))
			continue
		}
		response.Responses[qreq.RefID] = d.query(ctx, q.Text)
	}
	return response, nil
}

type queryRequest struct {
	RefID                string `json:"refId"`
	Text                 string `json:"queryText"`
	IntervalMilliseconds int    `json:"intervalMs"`
	MaxDataPoints        int    `json:"maxDataPoints"`
}

func (d *FlightSQLDatasource) query(ctx context.Context, sql string) backend.DataResponse {
	ctx = metadata.AppendToOutgoingContext(ctx, mdBucketName, d.database)

	info, err := d.client.Execute(ctx, sql)
	if err != nil {
		return backend.ErrDataResponse(backend.StatusInternal, fmt.Sprintf("flightsql: %s", err))
	}
	if len(info.Endpoint) != 1 {
		return backend.ErrDataResponse(backend.StatusInternal, fmt.Sprintf("unsupported endpoint count in response: %d", len(info.Endpoint)))
	}
	reader, err := d.client.DoGet(ctx, info.Endpoint[0].Ticket)
	if err != nil {
		return backend.ErrDataResponse(backend.StatusInternal, fmt.Sprintf("flightsql: %s", err))
	}
	defer reader.Release()

	// We've implemented our own conversions from Arrow to Data Frame, because
	// the Arrow dependency bundled with Grafana SDK is ancient. If we were to
	// use their functions, we'd end up writing the same amount of conversion
	// code to adapt the APIs.
	var resp backend.DataResponse
	frame := newFrame(reader.Schema(), sql)
	for reader.Next() {
		record := reader.Record()
		for i, col := range record.Columns() {
			copyData(frame.Fields[i], col)
		}

		if err := reader.Err(); err != nil && !errors.Is(err, io.EOF) {
			resp.Error = err
			break
		}
	}
	resp.Frames = append(resp.Frames, frame)
	return resp
}

// CheckHealth handles health checks sent from Grafana to the plugin.
// The main use case for these health checks is the test button on the
// datasource configuration page which allows users to verify that
// a datasource is working as expected.
func (d *FlightSQLDatasource) CheckHealth(ctx context.Context, req *backend.CheckHealthRequest) (*backend.CheckHealthResult, error) {
	if resp := d.query(ctx, "select 1"); resp.Error != nil {
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: fmt.Sprintf("ERROR: %s", resp.Error),
		}, nil
	}
	return &backend.CheckHealthResult{
		Status:  backend.HealthStatusOk,
		Message: "OK",
	}, nil
}

type bearerToken struct {
	token string
}

func (t bearerToken) GetRequestMetadata(ctx context.Context, in ...string) (map[string]string, error) {
	return map[string]string{
		"authorization": "Bearer " + t.token,
	}, nil
}

func (bearerToken) RequireTransportSecurity() bool {
	return true
}

type insecureBearerToken struct {
	token string
}

func (t insecureBearerToken) GetRequestMetadata(ctx context.Context, in ...string) (map[string]string, error) {
	return map[string]string{
		"authorization": "Bearer " + t.token,
	}, nil
}

func (insecureBearerToken) RequireTransportSecurity() bool {
	return false
}
