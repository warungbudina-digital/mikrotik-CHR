package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	pb "grpc-server/proto"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

// ─────────────────────────────────────────────
// Job store (in-memory — ganti Redis untuk production)
// ─────────────────────────────────────────────

type Job struct {
	ID        string
	Platform  pb.Platform
	URL       string
	Profile   string
	Status    pb.JobStatus
	MQTTTopic string
	Error     string
	CreatedAt int64
	UpdatedAt int64
}

type JobStore struct {
	mu   sync.RWMutex
	jobs map[string]*Job
}

func newJobStore() *JobStore { return &JobStore{jobs: make(map[string]*Job)} }

func (s *JobStore) set(j *Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[j.ID] = j
}

func (s *JobStore) get(id string) (*Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	return j, ok
}

func (s *JobStore) list(filter pb.JobStatus) []*Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		if filter == pb.JobStatus_JOB_STATUS_UNSPECIFIED || j.Status == filter {
			out = append(out, j)
		}
	}
	return out
}

// ─────────────────────────────────────────────
// gRPC server implementation
// ─────────────────────────────────────────────

type orchestratorServer struct {
	pb.UnimplementedOrchestratorServer
	startTime     time.Time
	store         *JobStore
	mqttClient    mqtt.Client
	browserURL    string
	browserAPIKey string // Bearer token untuk browser REST API (Phase 5)
	httpClient    *http.Client
}

func (s *orchestratorServer) Ping(_ context.Context, _ *pb.PingRequest) (*pb.PingResponse, error) {
	return &pb.PingResponse{
		Version:       "1.0.0",
		UptimeSeconds: int64(time.Since(s.startTime).Seconds()),
	}, nil
}

func (s *orchestratorServer) SubmitJob(_ context.Context, req *pb.SubmitJobRequest) (*pb.SubmitJobResponse, error) {
	if req.Url == "" {
		return nil, status.Error(codes.InvalidArgument, "url wajib diisi")
	}
	if req.Platform == pb.Platform_PLATFORM_UNSPECIFIED {
		return nil, status.Error(codes.InvalidArgument, "platform wajib diisi")
	}

	jobID := fmt.Sprintf("job-%d", time.Now().UnixNano())
	topic := fmt.Sprintf("scraper/results/%s", jobID)
	now := time.Now().Unix()

	job := &Job{
		ID:        jobID,
		Platform:  req.Platform,
		URL:       req.Url,
		Profile:   req.BrowserProfile,
		Status:    pb.JobStatus_JOB_STATUS_PENDING,
		MQTTTopic: topic,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.store.set(job)

	slog.Info("job submitted",
		"id", jobID,
		"platform", req.Platform.String(),
		"url", req.Url,
		"profile", req.BrowserProfile,
	)

	go s.executeJob(job)

	return &pb.SubmitJobResponse{
		JobId:     jobID,
		Status:    pb.JobStatus_JOB_STATUS_PENDING,
		MqttTopic: topic,
	}, nil
}

func (s *orchestratorServer) GetJob(_ context.Context, req *pb.GetJobRequest) (*pb.GetJobResponse, error) {
	job, ok := s.store.get(req.JobId)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "job tidak ditemukan: %s", req.JobId)
	}
	return jobToProto(job), nil
}

func (s *orchestratorServer) ListJobs(_ context.Context, req *pb.ListJobsRequest) (*pb.ListJobsResponse, error) {
	jobs := s.store.list(req.FilterStatus)
	limit := int(req.Limit)
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if len(jobs) > limit {
		jobs = jobs[:limit]
	}
	out := make([]*pb.GetJobResponse, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, jobToProto(j))
	}
	return &pb.ListJobsResponse{Jobs: out, Total: int32(len(out))}, nil
}

func (s *orchestratorServer) CancelJob(_ context.Context, req *pb.CancelJobRequest) (*pb.CancelJobResponse, error) {
	job, ok := s.store.get(req.JobId)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "job tidak ditemukan: %s", req.JobId)
	}
	if job.Status == pb.JobStatus_JOB_STATUS_DONE || job.Status == pb.JobStatus_JOB_STATUS_FAILED {
		return nil, status.Errorf(codes.FailedPrecondition,
			"job sudah selesai dengan status: %s", job.Status.String())
	}
	job.Status = pb.JobStatus_JOB_STATUS_CANCELLED
	job.UpdatedAt = time.Now().Unix()
	s.store.set(job)
	slog.Info("job cancelled", "id", req.JobId)
	return &pb.CancelJobResponse{JobId: req.JobId, Status: pb.JobStatus_JOB_STATUS_CANCELLED}, nil
}

// ─────────────────────────────────────────────
// Job execution — panggil full-tool-browser Scraper API (Phase 3+)
// ─────────────────────────────────────────────

// platformName mengonversi proto enum ke string platform scraper.
// PLATFORM_INSTAGRAM → "instagram", dll.
func platformName(p pb.Platform) string {
	raw := p.String()                            // "PLATFORM_INSTAGRAM"
	raw = strings.TrimPrefix(raw, "PLATFORM_")   // "INSTAGRAM"
	return strings.ToLower(raw)                  // "instagram"
}

// browserDo membuat HTTP request ke browser VPS dengan API key header.
func (s *orchestratorServer) browserDo(method, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(method, s.browserURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.browserAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.browserAPIKey)
	}
	return s.httpClient.Do(req)
}

func (s *orchestratorServer) executeJob(job *Job) {
	s.updateJobStatus(job, pb.JobStatus_JOB_STATUS_RUNNING, "")

	profile := job.Profile
	if profile == "" {
		profile = "openclaw"
	}

	// ── 1. Submit ke browser scraper API ──────────────────────────────
	payload, _ := json.Marshal(map[string]any{
		"platform":    platformName(job.Platform),
		"targetUrl":   job.URL,
		"profileName": profile,
	})

	resp, err := s.browserDo(http.MethodPost, "/scraper/jobs", payload)
	if err != nil {
		s.failJob(job, "submit ke browser gagal: "+err.Error())
		return
	}
	defer resp.Body.Close()

	var submitResp struct {
		OK  bool `json:"ok"`
		Job struct {
			ID string `json:"id"`
		} `json:"job"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&submitResp); err != nil {
		s.failJob(job, "decode submit response: "+err.Error())
		return
	}
	if !submitResp.OK {
		s.failJob(job, "browser menolak job: "+submitResp.Error)
		return
	}

	browserJobID := submitResp.Job.ID
	slog.Info("browser job submitted", "grpcJob", job.ID, "browserJob", browserJobID)

	// ── 2. Poll sampai selesai (max 5 menit, interval 5 detik) ────────
	const maxWait      = 5 * time.Minute
	const pollInterval = 5 * time.Second
	deadline := time.Now().Add(maxWait)

	var finalData map[string]any
	for time.Now().Before(deadline) {
		time.Sleep(pollInterval)

		pollResp, err := s.browserDo(http.MethodGet, "/scraper/jobs/"+browserJobID, nil)
		if err != nil {
			slog.Warn("poll error", "grpcJob", job.ID, "error", err)
			continue
		}

		var pollData map[string]any
		json.NewDecoder(pollResp.Body).Decode(&pollData)
		pollResp.Body.Close()

		jobObj, _ := pollData["job"].(map[string]any)
		jobStatus, _ := jobObj["status"].(string)

		switch jobStatus {
		case "done":
			finalData = pollData
		case "failed":
			errMsg, _ := jobObj["error"].(string)
			s.failJob(job, "scraper gagal: "+errMsg)
			return
		}

		if finalData != nil {
			break
		}
	}

	if finalData == nil {
		s.failJob(job, fmt.Sprintf("timeout setelah %s menunggu browser job %s", maxWait, browserJobID))
		return
	}

	// ── 3. Publish hasil ke MQTT ──────────────────────────────────────
	s.publishResult(job, map[string]any{
		"job_id":         job.ID,
		"browser_job_id": browserJobID,
		"platform":       platformName(job.Platform),
		"url":            job.URL,
		"result":         finalData,
		"timestamp":      time.Now().Unix(),
	})

	s.updateJobStatus(job, pb.JobStatus_JOB_STATUS_DONE, "")
}

func (s *orchestratorServer) failJob(job *Job, msg string) {
	slog.Error("job gagal", "id", job.ID, "error", msg)
	s.updateJobStatus(job, pb.JobStatus_JOB_STATUS_FAILED, msg)
	s.publishResult(job, map[string]any{
		"job_id":    job.ID,
		"platform":  platformName(job.Platform),
		"status":    "failed",
		"error":     msg,
		"timestamp": time.Now().Unix(),
	})
}

func (s *orchestratorServer) publishResult(job *Job, payload map[string]any) {
	if s.mqttClient == nil || !s.mqttClient.IsConnected() {
		return
	}
	data, _ := json.Marshal(payload)
	token := s.mqttClient.Publish(job.MQTTTopic, 1, false, data)
	token.Wait()
	if err := token.Error(); err != nil {
		slog.Warn("mqtt publish gagal", "topic", job.MQTTTopic, "error", err)
	} else {
		slog.Info("result published", "topic", job.MQTTTopic)
	}
}

func (s *orchestratorServer) updateJobStatus(job *Job, st pb.JobStatus, errMsg string) {
	job.Status = st
	job.Error = errMsg
	job.UpdatedAt = time.Now().Unix()
	s.store.set(job)
	slog.Info("job status updated", "id", job.ID, "status", st.String())
}

func jobToProto(j *Job) *pb.GetJobResponse {
	return &pb.GetJobResponse{
		JobId:     j.ID,
		Status:    j.Status,
		MqttTopic: j.MQTTTopic,
		Error:     j.Error,
		CreatedAt: j.CreatedAt,
		UpdatedAt: j.UpdatedAt,
	}
}

// ─────────────────────────────────────────────
// MQTT
// ─────────────────────────────────────────────

func newMQTTClient(broker, user, pass string) mqtt.Client {
	opts := mqtt.NewClientOptions().
		AddBroker(broker).
		SetClientID("grpc-orchestrator").
		SetUsername(user).
		SetPassword(pass).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetOnConnectHandler(func(_ mqtt.Client) {
			slog.Info("MQTT connected", "broker", broker)
		}).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			slog.Warn("MQTT connection lost", "error", err)
		})

	c := mqtt.NewClient(opts)
	if token := c.Connect(); token.Wait() && token.Error() != nil {
		slog.Warn("MQTT initial connect failed — akan retry otomatis", "error", token.Error())
	}
	return c
}

// ─────────────────────────────────────────────
// Main
// ─────────────────────────────────────────────

func main() {
	grpcPort      := getenv("GRPC_PORT", "9090")
	browserURL    := getenv("BROWSER_URL", "http://10.10.0.2:8080")
	browserAPIKey := os.Getenv("BROWSER_API_KEY") // Bearer token Phase 5
	mqttBroker    := getenv("MQTT_BROKER", "tcp://mosquitto:1883")
	mqttUser      := getenv("MQTT_USERNAME", "browser-agent")
	mqttPass      := os.Getenv("MQTT_PASSWORD")

	slog.Info("starting orchestrator",
		"grpc_port", grpcPort,
		"browser_url", browserURL,
		"mqtt_broker", mqttBroker,
		"browser_auth", browserAPIKey != "",
	)

	srv := &orchestratorServer{
		startTime:     time.Now(),
		store:         newJobStore(),
		mqttClient:    newMQTTClient(mqttBroker, mqttUser, mqttPass),
		browserURL:    browserURL,
		browserAPIKey: browserAPIKey,
		httpClient:    &http.Client{Timeout: 10 * time.Minute}, // perlu tunggu scraping selesai
	}

	lis, err := net.Listen("tcp", ":"+grpcPort)
	if err != nil {
		slog.Error("listen failed", "error", err)
		os.Exit(1)
	}

	s := grpc.NewServer()
	pb.RegisterOrchestratorServer(s, srv)
	healthpb.RegisterHealthServer(s, health.NewServer())
	reflection.Register(s)

	slog.Info("gRPC server listening", "port", grpcPort)
	if err := s.Serve(lis); err != nil {
		slog.Error("serve failed", "error", err)
		os.Exit(1)
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
