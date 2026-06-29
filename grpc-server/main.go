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
	"sync"
	"time"

	pb "grpc-server/proto/v1"

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
	startTime  time.Time
	store      *JobStore
	mqttClient mqtt.Client
	browserURL string
	httpClient *http.Client
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
// Job execution — panggil full-tool-browser REST API
// ─────────────────────────────────────────────

func (s *orchestratorServer) executeJob(job *Job) {
	s.updateJobStatus(job, pb.JobStatus_JOB_STATUS_RUNNING, "")

	profile := job.Profile
	if profile == "" {
		profile = "openclaw"
	}

	payload, _ := json.Marshal(map[string]any{
		"action":  "navigate",
		"profile": profile,
		"url":     job.URL,
	})

	resp, err := s.httpClient.Post(
		s.browserURL+"/browser/request",
		"application/json",
		bytes.NewReader(payload),
	)
	if err != nil {
		slog.Error("browser request failed", "job", job.ID, "error", err)
		s.updateJobStatus(job, pb.JobStatus_JOB_STATUS_FAILED, err.Error())
		return
	}
	defer resp.Body.Close()

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		s.updateJobStatus(job, pb.JobStatus_JOB_STATUS_FAILED, "decode error: "+err.Error())
		return
	}

	resultJSON, _ := json.Marshal(map[string]any{
		"job_id":    job.ID,
		"platform":  job.Platform.String(),
		"url":       job.URL,
		"result":    result,
		"timestamp": time.Now().Unix(),
	})

	if s.mqttClient != nil && s.mqttClient.IsConnected() {
		token := s.mqttClient.Publish(job.MQTTTopic, 1, false, resultJSON)
		token.Wait()
		if err := token.Error(); err != nil {
			slog.Warn("mqtt publish failed", "topic", job.MQTTTopic, "error", err)
		} else {
			slog.Info("result published", "topic", job.MQTTTopic)
		}
	}

	s.updateJobStatus(job, pb.JobStatus_JOB_STATUS_DONE, "")
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
	grpcPort := getenv("GRPC_PORT", "9090")
	browserURL := getenv("BROWSER_URL", "http://10.10.0.2:8080")
	mqttBroker := getenv("MQTT_BROKER", "tcp://mosquitto:1883")
	mqttUser := getenv("MQTT_USERNAME", "browser-agent")
	mqttPass := os.Getenv("MQTT_PASSWORD")

	slog.Info("starting orchestrator",
		"grpc_port", grpcPort,
		"browser_url", browserURL,
		"mqtt_broker", mqttBroker,
	)

	srv := &orchestratorServer{
		startTime:  time.Now(),
		store:      newJobStore(),
		mqttClient: newMQTTClient(mqttBroker, mqttUser, mqttPass),
		browserURL: browserURL,
		httpClient: &http.Client{Timeout: 30 * time.Second},
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
