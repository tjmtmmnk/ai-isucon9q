package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	IsucariAPIToken = "Bearer 75ugk2m37a750fwir5xr-22l6h4wmue1bwrubzwd0"

	userAgent = "isucon9-qualify-webapp"
)

// apiHTTPClient is a custom HTTP client optimized for external API calls.
// Why not use http.DefaultClient: DefaultClient has no timeout and limited
// connection pooling. This custom client reduces connection overhead and
// improves latency for repeated API calls.
var apiHTTPClient = &http.Client{
	Timeout: 5 * time.Second,
	Transport: &http.Transport{
		// Connection pool settings for high concurrency
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 50,
		MaxConnsPerHost:     100,
		// Timeouts for connection establishment
		DialContext: (&net.Dialer{
			Timeout:   3 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		// Keep-alive settings
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   3 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
}

type APIPaymentServiceTokenReq struct {
	ShopID string `json:"shop_id"`
	Token  string `json:"token"`
	APIKey string `json:"api_key"`
	Price  int    `json:"price"`
}

type APIPaymentServiceTokenRes struct {
	Status string `json:"status"`
}

type APIShipmentCreateReq struct {
	ToAddress   string `json:"to_address"`
	ToName      string `json:"to_name"`
	FromAddress string `json:"from_address"`
	FromName    string `json:"from_name"`
}

type APIShipmentCreateRes struct {
	ReserveID   string `json:"reserve_id"`
	ReserveTime int64  `json:"reserve_time"`
}

type APIShipmentRequestReq struct {
	ReserveID string `json:"reserve_id"`
}

type APIShipmentStatusRes struct {
	Status      string `json:"status"`
	ReserveTime int64  `json:"reserve_time"`
}

type APIShipmentStatusReq struct {
	ReserveID string `json:"reserve_id"`
}

func APIPaymentToken(ctx context.Context, paymentURL string, param *APIPaymentServiceTokenReq) (*APIPaymentServiceTokenRes, error) {
	ctx, span := tracer.Start(ctx, "APIPaymentToken",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			semconv.HTTPURLKey.String(paymentURL+"/token"),
			semconv.HTTPMethodKey.String(http.MethodPost),
			semconv.PeerServiceKey.String("payment"),
		),
	)
	defer span.End()

	b, _ := json.Marshal(param)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, paymentURL+"/token", bytes.NewBuffer(b))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", "application/json")
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))

	res, err := apiHTTPClient.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	defer res.Body.Close()

	span.SetAttributes(semconv.HTTPStatusCodeKey.Int(res.StatusCode))

	if res.StatusCode != http.StatusOK {
		b, err := io.ReadAll(res.Body)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("failed to read res.Body and the status code of the response from shipment service was not 200: %v", err)
		}
		err = fmt.Errorf("status code: %d; body: %s", res.StatusCode, b)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	pstr := &APIPaymentServiceTokenRes{}
	err = json.NewDecoder(res.Body).Decode(pstr)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	return pstr, nil
}

func APIShipmentCreate(ctx context.Context, shipmentURL string, param *APIShipmentCreateReq) (*APIShipmentCreateRes, error) {
	ctx, span := tracer.Start(ctx, "APIShipmentCreate",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			semconv.HTTPURLKey.String(shipmentURL+"/create"),
			semconv.HTTPMethodKey.String(http.MethodPost),
			semconv.PeerServiceKey.String("shipment"),
		),
	)
	defer span.End()

	b, _ := json.Marshal(param)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, shipmentURL+"/create", bytes.NewBuffer(b))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", IsucariAPIToken)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))

	res, err := apiHTTPClient.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	defer res.Body.Close()

	span.SetAttributes(semconv.HTTPStatusCodeKey.Int(res.StatusCode))

	if res.StatusCode != http.StatusOK {
		b, err := io.ReadAll(res.Body)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("failed to read res.Body and the status code of the response from shipment service was not 200: %v", err)
		}
		err = fmt.Errorf("status code: %d; body: %s", res.StatusCode, b)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	scr := &APIShipmentCreateRes{}
	err = json.NewDecoder(res.Body).Decode(&scr)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	return scr, nil
}

func APIShipmentRequest(ctx context.Context, shipmentURL string, param *APIShipmentRequestReq) ([]byte, error) {
	ctx, span := tracer.Start(ctx, "APIShipmentRequest",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			semconv.HTTPURLKey.String(shipmentURL+"/request"),
			semconv.HTTPMethodKey.String(http.MethodPost),
			semconv.PeerServiceKey.String("shipment"),
		),
	)
	defer span.End()

	b, _ := json.Marshal(param)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, shipmentURL+"/request", bytes.NewBuffer(b))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", IsucariAPIToken)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))

	res, err := apiHTTPClient.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	defer res.Body.Close()

	span.SetAttributes(semconv.HTTPStatusCodeKey.Int(res.StatusCode))

	if res.StatusCode != http.StatusOK {
		b, err := io.ReadAll(res.Body)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("failed to read res.Body and the status code of the response from shipment service was not 200: %v", err)
		}
		err = fmt.Errorf("status code: %d; body: %s", res.StatusCode, b)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	return io.ReadAll(res.Body)
}

func APIShipmentStatus(ctx context.Context, shipmentURL string, param *APIShipmentStatusReq) (*APIShipmentStatusRes, error) {
	ctx, span := tracer.Start(ctx, "APIShipmentStatus",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			semconv.HTTPURLKey.String(shipmentURL+"/status"),
			semconv.HTTPMethodKey.String(http.MethodGet),
			semconv.PeerServiceKey.String("shipment"),
		),
	)
	defer span.End()

	b, _ := json.Marshal(param)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, shipmentURL+"/status", bytes.NewBuffer(b))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", IsucariAPIToken)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))

	res, err := apiHTTPClient.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	defer res.Body.Close()

	span.SetAttributes(semconv.HTTPStatusCodeKey.Int(res.StatusCode))

	if res.StatusCode != http.StatusOK {
		b, err := io.ReadAll(res.Body)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("failed to read res.Body and the status code of the response from shipment service was not 200: %v", err)
		}
		err = fmt.Errorf("status code: %d; body: %s", res.StatusCode, b)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	ssr := &APIShipmentStatusRes{}
	err = json.NewDecoder(res.Body).Decode(&ssr)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	return ssr, nil
}
