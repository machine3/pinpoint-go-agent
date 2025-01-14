package pinpoint

import (
	"context"
	"fmt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"math"
	"math/rand"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/golang/protobuf/ptypes/wrappers"
	pb "github.com/pinpoint-apm/pinpoint-go-agent/protobuf"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

func grpcMetadataContext(agent Agent, socketId int64) context.Context {
	m := map[string]string{}

	m["agentid"] = agent.Config().AgentId
	m["applicationname"] = agent.Config().ApplicationName
	m["starttime"] = strconv.FormatInt(agent.StartTime(), 10)

	if socketId > 0 {
		m["socketid"] = strconv.FormatInt(socketId, 10)
	}

	md := metadata.New(m)
	return metadata.NewOutgoingContext(context.Background(), md)
}

func backOffSleep(attempt int) {
	base := float64(1 * time.Second)
	max := float64(60 * time.Second)

	dur := base * math.Pow(2, float64(attempt))
	if dur > max {
		dur = max
	}

	jitter := time.Duration(rand.Float64()*(dur-base) + base)
	time.Sleep(jitter)
}

type AgentGrpcClient interface {
	RequestAgentInfo(ctx context.Context, agentinfo *pb.PAgentInfo) (*pb.PResult, error)
	PingSession(ctx context.Context) (pb.Agent_PingSessionClient, error)
}

type agentGrpcClient struct {
	client pb.AgentClient
}

func (agentGrpcClient *agentGrpcClient) RequestAgentInfo(ctx context.Context, agentinfo *pb.PAgentInfo) (*pb.PResult, error) {
	result, err := agentGrpcClient.client.RequestAgentInfo(ctx, agentinfo)
	return result, err
}

func (agentGrpcClient *agentGrpcClient) PingSession(ctx context.Context) (pb.Agent_PingSessionClient, error) {
	session, err := agentGrpcClient.client.PingSession(ctx)
	return session, err
}

type MetaGrpcClient interface {
	RequestApiMetaData(ctx context.Context, in *pb.PApiMetaData) (*pb.PResult, error)
	RequestSqlMetaData(ctx context.Context, in *pb.PSqlMetaData) (*pb.PResult, error)
	RequestStringMetaData(ctx context.Context, in *pb.PStringMetaData) (*pb.PResult, error)
}

type metaGrpcClient struct {
	client pb.MetadataClient
}

func (metaGrpcClient *metaGrpcClient) RequestApiMetaData(ctx context.Context, in *pb.PApiMetaData) (*pb.PResult, error) {
	result, err := metaGrpcClient.client.RequestApiMetaData(ctx, in)
	return result, err
}

func (metaGrpcClient *metaGrpcClient) RequestSqlMetaData(ctx context.Context, in *pb.PSqlMetaData) (*pb.PResult, error) {
	result, err := metaGrpcClient.client.RequestSqlMetaData(ctx, in)
	return result, err
}

func (metaGrpcClient *metaGrpcClient) RequestStringMetaData(ctx context.Context, in *pb.PStringMetaData) (*pb.PResult, error) {
	result, err := metaGrpcClient.client.RequestStringMetaData(ctx, in)
	return result, err
}

type agentGrpc struct {
	agentConn      *grpc.ClientConn
	agentClient    AgentGrpcClient
	metadataClient MetaGrpcClient
	pingSocketId   int64
	agent          Agent
}

var kacp = keepalive.ClientParameters{
	Time:                5 * time.Minute,
	Timeout:             30 * time.Minute,
	PermitWithoutStream: true,
}

func connectToCollectorWithRetry(serverAddr string, opts []grpc.DialOption) (*grpc.ClientConn, error) {
	var conn *grpc.ClientConn
	var err error

	for n := 1; n < 100; n++ {
		log("grpc").Infof("connect to collector: %s", serverAddr)
		conn, err = grpc.Dial(serverAddr, opts...)
		if err == nil {
			break
		}
		log("grpc").Errorf("fail to dial - %v", err)
		backOffSleep(n)
	}

	return conn, err
}

func newAgentGrpc(agent Agent) (*agentGrpc, error) {
	var opts []grpc.DialOption

	opts = append(opts, grpc.WithInsecure())
	opts = append(opts, grpc.WithKeepaliveParams(kacp))
	opts = append(opts, grpc.WithBlock())
	opts = append(opts, grpc.WithTimeout(3*time.Second))

	serverAddr := fmt.Sprintf("%s:%d", agent.Config().Collector.Host, agent.Config().Collector.AgentPort)
	conn, err := connectToCollectorWithRetry(serverAddr, opts)
	if err != nil {
		return nil, err
	}

	agentClient := agentGrpcClient{pb.NewAgentClient(conn)}
	metadataClient := metaGrpcClient{pb.NewMetadataClient(conn)}
	return &agentGrpc{conn, &agentClient, &metadataClient, 0, agent}, nil
}

func makeAgentInfo(agent Agent) (context.Context, *pb.PAgentInfo) {
	var agentinfo pb.PAgentInfo
	agentinfo.AgentVersion = AgentVersion

	hostname, err := os.Hostname()
	if err != nil {
		log("grpc").Errorf("fail to os.Hostname() - %v", err)
		hostname = "unknown"
	}

	agentinfo.Hostname = hostname
	agentinfo.Ip = getOutboundIP().String()
	agentinfo.ServiceType = agent.Config().ApplicationType
	agentinfo.Container = agent.Config().IsContainer

	var svrMeta pb.PServerMetaData
	svrMeta.ServerInfo = "Go Agent"
	agentinfo.ServerMetaData = &svrMeta

	log("grpc").Infof("send agent information: %s", agentinfo.String())

	ctx := grpcMetadataContext(agent, -1)

	return ctx, &agentinfo
}

func (agentGrpc *agentGrpc) sendAgentInfo() error {
	ctx, agentinfo := makeAgentInfo(agentGrpc.agent)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_, err := agentGrpc.agentClient.RequestAgentInfo(ctx, agentinfo)
	if err != nil {
		log("grpc").Errorf("fail to call RequestAgentInfo() - %v", err)
	}

	return err
}

func (agentGrpc *agentGrpc) sendApiMetadata(apiId int32, api string, line int, apiType int) error {
	var apimeta pb.PApiMetaData
	apimeta.ApiId = apiId
	apimeta.ApiInfo = api
	apimeta.Line = int32(line)
	apimeta.Type = int32(apiType)

	log("grpc").Infof("send API metadata: %s", apimeta.String())

	ctx := grpcMetadataContext(agentGrpc.agent, -1)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_, err := agentGrpc.metadataClient.RequestApiMetaData(ctx, &apimeta)
	if err != nil {
		log("grpc").Errorf("fail to call RequestApiMetaData() - %v", err)
	}

	return err
}

func (agentGrpc *agentGrpc) sendStringMetadata(strId int32, str string) error {
	var strmeta pb.PStringMetaData
	strmeta.StringId = strId
	strmeta.StringValue = str

	log("grpc").Infof("send string metadata: %s", strmeta.String())

	ctx := grpcMetadataContext(agentGrpc.agent, -1)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_, err := agentGrpc.metadataClient.RequestStringMetaData(ctx, &strmeta)
	if err != nil {
		log("grpc").Errorf("fail to call RequestStringMetaData() - %v", err)
	}

	return err
}

func (agentGrpc *agentGrpc) sendSqlMetadata(sqlId int32, sql string) error {
	var sqlmeta pb.PSqlMetaData
	sqlmeta.SqlId = sqlId
	sqlmeta.Sql = sql

	log("grpc").Infof("send SQL metadata: %s", sqlmeta.String())

	ctx := grpcMetadataContext(agentGrpc.agent, -1)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_, err := agentGrpc.metadataClient.RequestSqlMetaData(ctx, &sqlmeta)
	if err != nil {
		log("grpc").Errorf("fail to call RequestSqlMetaData() - %v", err)
	}

	return err
}

type pingStream struct {
	stream pb.Agent_PingSessionClient
}

func (agentGrpc *agentGrpc) newPingStream() *pingStream {
	agentGrpc.pingSocketId++
	ctx := grpcMetadataContext(agentGrpc.agent, agentGrpc.pingSocketId)
	//ctx, _ = context.WithTimeout(ctx, 30 * time.Second)
	//defer cancel()

	stream, err := agentGrpc.agentClient.PingSession(ctx)
	if err != nil {
		log("grpc").Errorf("fail to make ping stream - %v", err)
		return &pingStream{nil}
	}

	return &pingStream{stream}
}

func (agentGrpc *agentGrpc) newPingStreamWithRetry() *pingStream {
	for n := 1; n < 100; n++ {
		if !agentGrpc.agent.Enable() {
			break
		}

		s := agentGrpc.newPingStream()
		if s.stream != nil {
			log("grpc").Info("success to make ping stream: ", n)
			return s
		}
		backOffSleep(n)
	}

	return &pingStream{nil}
}

var ping = pb.PPing{}

func (s *pingStream) sendPing() error {
	if s.stream == nil {
		return status.Errorf(codes.Unavailable, "ping stream is nil")
	}

	return s.stream.Send(&ping)
}

func (s *pingStream) close() {
	if s.stream != nil {
		s.stream.CloseSend()
		s.stream = nil
	}
}

func (agentGrpc *agentGrpc) close() {
	agentGrpc.agentConn.Close()
}

func getOutboundIP() net.IP {
	conn, _ := net.Dial("udp", "8.8.8.8:80")
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)

	return localAddr.IP
}

type SpanGrpcClient interface {
	SendSpan(ctx context.Context) (pb.Span_SendSpanClient, error)
}

type spanGrpcClient struct {
	client pb.SpanClient
}

func (spanGrpcClient *spanGrpcClient) SendSpan(ctx context.Context) (pb.Span_SendSpanClient, error) {
	return spanGrpcClient.client.SendSpan(ctx)
}

type spanGrpc struct {
	spanConn   *grpc.ClientConn
	spanClient SpanGrpcClient
	stream     SpanStreamInvoker
	agent      Agent
}

type SpanStreamInvoker interface {
	Send(*pb.PSpanMessage) error
	CloseAndRecv() error
	CloseSend() error
}

type spanStreamInvoker struct {
	stream pb.Span_SendSpanClient
}

func (invoker *spanStreamInvoker) Send(span *pb.PSpanMessage) error {
	return invoker.stream.Send(span)
}

func (invoker *spanStreamInvoker) CloseAndRecv() error {
	_, err := invoker.stream.CloseAndRecv()
	return err
}

func (invoker *spanStreamInvoker) CloseSend() error {
	return invoker.stream.CloseSend()
}

type spanStream struct {
	stream SpanStreamInvoker
}

func newSpanGrpc(agent Agent) (*spanGrpc, error) {
	var opts []grpc.DialOption

	opts = append(opts, grpc.WithInsecure())
	opts = append(opts, grpc.WithKeepaliveParams(kacp))
	opts = append(opts, grpc.WithBlock())
	opts = append(opts, grpc.WithTimeout(3*time.Second))

	serverAddr := fmt.Sprintf("%s:%d", agent.Config().Collector.Host, agent.Config().Collector.SpanPort)
	conn, err := connectToCollectorWithRetry(serverAddr, opts)
	if err != nil {
		return nil, err
	}

	client := spanGrpcClient{pb.NewSpanClient(conn)}
	return &spanGrpc{conn, &client, nil, agent}, nil
}

func (spanGrpc *spanGrpc) close() {
	spanGrpc.spanConn.Close()
}

func (spanGrpc *spanGrpc) newSpanStream() *spanStream {
	ctx := grpcMetadataContext(spanGrpc.agent, -1)
	//ctx, _ = context.WithTimeout(ctx, 30 * time.Second)
	//defer cancel()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := spanGrpc.spanClient.SendSpan(ctx)
	if err != nil {
		log("grpc").Errorf("fail to make span stream - %v", err)
		return &spanStream{nil}
	}

	return &spanStream{&spanStreamInvoker{stream}}
}

func (spanGrpc *spanGrpc) newSpanStreamWithRetry() *spanStream {
	for n := 1; n < 100; n++ {
		if !spanGrpc.agent.Enable() {
			break
		}

		s := spanGrpc.newSpanStream()
		if s.stream != nil {
			log("grpc").Info("success to make span stream: ", n)
			return s
		}
		backOffSleep(n)
	}

	return &spanStream{nil}
}

func (s *spanStream) setStreamInvoker(invoker SpanStreamInvoker) {
	s.stream = invoker
}

func (s *spanStream) close() {
	if s.stream == nil {
		return
	}

	err := s.stream.CloseAndRecv()
	if err != nil {
		log("grpc").Errorf("fail to close span stream - %v", err)
	}
	s.stream = nil
}

func (s *spanStream) sendSpan(span *span) error {
	var gspan *pb.PSpanMessage

	if s.stream == nil {
		return status.Errorf(codes.Unavailable, "span stream is nil")
	}

	if span.asyncId == 0 {
		gspan = makePSpan(span)
	} else {
		gspan = makePSpanChunk(span)
	}

	log("grpc").Debug("PSpanMessage: ", gspan.String())

	return s.stream.Send(gspan)
}

func makePSpan(span *span) *pb.PSpanMessage {
	span.annotations.AppendString(12, span.operationName)

	spanEventList := make([]*pb.PSpanEvent, 0)
	for _, event := range span.spanEvents {
		aSpanEvent := makePSpanEvent(event)
		spanEventList = append(spanEventList, aSpanEvent)
	}

	gspan := &pb.PSpanMessage{
		Field: &pb.PSpanMessage_Span{
			Span: &pb.PSpan{
				Version: 1,
				TransactionId: &pb.PTransactionId{
					AgentId:        span.txId.AgentId,
					AgentStartTime: span.txId.StartTime,
					Sequence:       span.txId.Sequence,
				},
				SpanId:       span.spanId,
				ParentSpanId: span.parentSpanId,
				StartTime:    span.startTime.UnixNano() / int64(time.Millisecond),
				Elapsed:      int32(toMilliseconds(span.duration)),
				ServiceType:  span.serviceType,
				AcceptEvent: &pb.PAcceptEvent{
					Rpc:        span.rpcName,
					EndPoint:   span.endPoint,
					RemoteAddr: span.remoteAddr,
					ParentInfo: nil,
				},
				Annotation:             span.annotations.list,
				Flag:                   int32(span.flags),
				SpanEvent:              spanEventList,
				Err:                    int32(span.err),
				ExceptionInfo:          nil, //TODO
				ApplicationServiceType: span.agent.Config().ApplicationType,
				LoggingTransactionInfo: span.loggingInfo,
			},
		},
	}

	if span.parentAppName != "" {
		pinfo := &pb.PParentInfo{
			ParentApplicationName: span.parentAppName,
			ParentApplicationType: int32(span.parentAppType),
			AcceptorHost:          span.acceptorHost,
		}

		gspan.GetSpan().AcceptEvent.ParentInfo = pinfo
	}

	if span.apiId > 0 {
		gspan.GetSpan().ApiId = span.apiId
	}

	return gspan
}

func makePSpanChunk(span *span) *pb.PSpanMessage {
	spanEventList := make([]*pb.PSpanEvent, 0)
	for _, event := range span.spanEvents {
		aSpanEvent := makePSpanEvent(event)
		spanEventList = append(spanEventList, aSpanEvent)
	}

	gspan := &pb.PSpanMessage{
		Field: &pb.PSpanMessage_SpanChunk{
			SpanChunk: &pb.PSpanChunk{
				Version: 1,
				TransactionId: &pb.PTransactionId{
					AgentId:        span.txId.AgentId,
					AgentStartTime: span.txId.StartTime,
					Sequence:       span.txId.Sequence,
				},
				SpanId:                 span.spanId,
				KeyTime:                span.startTime.UnixNano() / int64(time.Millisecond),
				EndPoint:               span.endPoint,
				SpanEvent:              spanEventList,
				ApplicationServiceType: span.agent.Config().ApplicationType,
				LocalAsyncId: &pb.PLocalAsyncId{
					AsyncId:  span.asyncId,
					Sequence: span.asyncSequence,
				},
			},
		},
	}

	return gspan
}

func makePSpanEvent(event *spanEvent) *pb.PSpanEvent {
	if event.apiId == 0 && event.operationName != "" {
		event.annotations.AppendString(12, event.operationName)
	}

	aSpanEvent := pb.PSpanEvent{
		Sequence:      event.sequence,
		Depth:         event.depth,
		StartElapsed:  int32(toMilliseconds(event.startElapsed)),
		EndElapsed:    int32(toMilliseconds(event.duration)),
		ServiceType:   event.serviceType,
		Annotation:    event.annotations.list,
		ApiId:         event.apiId,
		ExceptionInfo: nil,
		NextEvent:     nil,
		AsyncEvent:    event.asyncId,
	}

	if event.errorString != "" {
		aSpanEvent.ExceptionInfo = &pb.PIntStringValue{
			IntValue:    event.errorFuncId,
			StringValue: &wrappers.StringValue{Value: event.errorString},
		}
	}

	if event.destinationId != "" {
		next := &pb.PNextEvent{
			Field: &pb.PNextEvent_MessageEvent{
				MessageEvent: &pb.PMessageEvent{
					NextSpanId:    event.nextSpanId,
					EndPoint:      event.endPoint,
					DestinationId: event.destinationId,
				},
			},
		}

		aSpanEvent.NextEvent = next
	}

	return &aSpanEvent
}

func (s *spanStream) sendSpanFinish() {
	s.stream.CloseSend()
}

type StatGrpcClient interface {
	SendAgentStat(ctx context.Context) (pb.Stat_SendAgentStatClient, error)
}

type statGrpcClient struct {
	client pb.StatClient
}

func (statGrpcClient *statGrpcClient) SendAgentStat(ctx context.Context) (pb.Stat_SendAgentStatClient, error) {
	return statGrpcClient.client.SendAgentStat(ctx)
}

type statGrpc struct {
	statConn   *grpc.ClientConn
	statClient StatGrpcClient
	stream     StatStreamInvoker
	agent      Agent
}

type StatStreamInvoker interface {
	Send(*pb.PStatMessage) error
	CloseAndRecv() error
	CloseSend() error
}

type statStreamInvoker struct {
	stream pb.Stat_SendAgentStatClient
}

func (invoker *statStreamInvoker) Send(stat *pb.PStatMessage) error {
	return invoker.stream.Send(stat)
}

func (invoker *statStreamInvoker) CloseAndRecv() error {
	_, err := invoker.stream.CloseAndRecv()
	return err
}

func (invoker *statStreamInvoker) CloseSend() error {
	return invoker.stream.CloseSend()
}

type statStream struct {
	stream StatStreamInvoker
}

func newStatGrpc(agent Agent) (*statGrpc, error) {
	var opts []grpc.DialOption

	opts = append(opts, grpc.WithInsecure())
	opts = append(opts, grpc.WithKeepaliveParams(kacp))
	opts = append(opts, grpc.WithBlock())
	opts = append(opts, grpc.WithTimeout(3*time.Second))

	serverAddr := fmt.Sprintf("%s:%d", agent.Config().Collector.Host, agent.Config().Collector.StatPort)
	conn, err := connectToCollectorWithRetry(serverAddr, opts)
	if err != nil {
		return nil, err
	}

	client := &statGrpcClient{pb.NewStatClient(conn)}
	return &statGrpc{conn, client, nil, agent}, nil
}

func (statGrpc *statGrpc) close() {
	statGrpc.statConn.Close()
}

func (statGrpc *statGrpc) newStatStream() *statStream {
	ctx := grpcMetadataContext(statGrpc.agent, -1)
	//ctx, _ = context.WithTimeout(ctx, 30 * time.Second)
	//defer cancel()

	stream, err := statGrpc.statClient.SendAgentStat(ctx)
	if err != nil {
		log("grpc").Errorf("fail to make stat stream - %v", err)
		return &statStream{nil}
	}

	return &statStream{&statStreamInvoker{stream}}
}

func (statGrpc *statGrpc) newStatStreamWithRetry() *statStream {
	for n := 1; n < 100; n++ {
		if !statGrpc.agent.Enable() {
			break
		}

		s := statGrpc.newStatStream()
		if s.stream != nil {
			log("grpc").Info("success to make stat stream: ", n)
			return s
		}
		backOffSleep(n)
	}

	return &statStream{nil}
}

func (s *statStream) setStreamInvoker(invoker StatStreamInvoker) {
	s.stream = invoker
}

func (s *statStream) close() {
	if s.stream == nil {
		return
	}

	err := s.stream.CloseAndRecv()
	if err != nil {
		log("grpc").Errorf("fail to close stat stream - %v", err)
	}
	s.stream = nil
}

func (s *statStream) sendStats(stats []*inspectorStats) error {
	var gstats *pb.PStatMessage

	if s.stream == nil {
		return status.Errorf(codes.Unavailable, "stat stream is nil")
	}

	gstats = &pb.PStatMessage{
		Field: &pb.PStatMessage_AgentStatBatch{
			AgentStatBatch: &pb.PAgentStatBatch{
				AgentStat: nil,
			},
		},
	}

	var as []*pb.PAgentStat
	for _, s := range stats {
		as = append(as, makePAgentStat(s))
	}
	gstats.GetAgentStatBatch().AgentStat = as

	log("grpc").Debug("PStatMessage: ", gstats.String())

	return s.stream.Send(gstats)
}

func makePAgentStat(stat *inspectorStats) *pb.PAgentStat {
	return &pb.PAgentStat{
		Timestamp:       stat.sampleTime.UnixNano() / int64(time.Millisecond),
		CollectInterval: 5000,
		Gc: &pb.PJvmGc{
			Type:                 1,
			JvmMemoryHeapUsed:    stat.heapAlloc,
			JvmMemoryHeapMax:     stat.heapMax,
			JvmMemoryNonHeapUsed: stat.nonHeapAlloc,
			JvmMemoryNonHeapMax:  stat.nonHeapAlloc + 1000,
			JvmGcOldCount:        stat.gcNum,
			JvmGcOldTime:         stat.gcTime,
			JvmGcDetailed:        nil,
		},
		CpuLoad: &pb.PCpuLoad{
			JvmCpuLoad:    stat.cpuUserTime,
			SystemCpuLoad: stat.cpuSysTime,
		},
		Transaction: &pb.PTransaction{
			SampledNewCount:            stat.sampleNew,
			SampledContinuationCount:   stat.sampleCont,
			UnsampledNewCount:          stat.unSampleNew,
			UnsampledContinuationCount: stat.unSampleCont,
			SkippedNewCount:            stat.skipNew,
			SkippedContinuationCount:   stat.skipCont,
		},
		ActiveTrace: &pb.PActiveTrace{
			Histogram: &pb.PActiveTraceHistogram{
				Version:             1,
				HistogramSchemaType: 2, //NORMAL SCHEMA
				ActiveTraceCount:    stat.activeSpan,
			},
		},
		DataSourceList: nil,
		ResponseTime: &pb.PResponseTime{
			Avg: stat.responseAvg,
			Max: stat.responseMax,
		},
		Deadlock:       nil,
		FileDescriptor: nil,
		DirectBuffer:   nil,
		Metadata:       "",
	}
}

type cmdGrpc struct {
	agentConn *grpc.ClientConn
	cmdClient pb.ProfilerCommandServiceClient
	agent     Agent
}

type cmdStream struct {
	stream pb.ProfilerCommandService_HandleCommandClient
	cmdReq *pb.PCmdRequest
}

func newCommandGrpc(agent Agent) (*cmdGrpc, error) {
	var opts []grpc.DialOption

	opts = append(opts, grpc.WithInsecure())
	opts = append(opts, grpc.WithKeepaliveParams(kacp))
	opts = append(opts, grpc.WithBlock())
	opts = append(opts, grpc.WithTimeout(3*time.Second))

	serverAddr := fmt.Sprintf("%s:%d", agent.Config().Collector.Host, agent.Config().Collector.AgentPort)

	log("grpc").Infof("connect to agent collector: %s", serverAddr)
	conn, err := grpc.Dial(serverAddr, opts...)
	if err != nil {
		log("grpc").Errorf("fail to dial - %v", err)
		return nil, err
	}

	cmdClient := pb.NewProfilerCommandServiceClient(conn)
	return &cmdGrpc{conn, cmdClient, agent}, nil
}

func (cmdGrpc *cmdGrpc) newHandleCommandStream() *cmdStream {
	ctx := grpcMetadataContext(cmdGrpc.agent, -1)
	//ctx, _ = context.WithTimeout(ctx, 30 * time.Second)
	//defer cancel()

	stream, err := cmdGrpc.cmdClient.HandleCommand(ctx)
	if err != nil {
		log("grpc").Errorf("fail to make command stream - %v", err)
		return &cmdStream{nil, nil}
	}

	return &cmdStream{stream, nil}
}

func (cmdGrpc *cmdGrpc) newCommandStreamWithRetry() *cmdStream {
	for n := 1; n < 100; n++ {
		if !cmdGrpc.agent.Enable() {
			break
		}

		s := cmdGrpc.newHandleCommandStream()
		if s.stream != nil {
			log("grpc").Info("success to make command stream: ", n)
			return s
		}
		backOffSleep(n)
	}

	return &cmdStream{nil, nil}
}

func (s *cmdStream) close() {
	if s.stream == nil {
		return
	}

	err := s.stream.CloseSend()
	if err != nil {
		log("grpc").Errorf("fail to close command stream - %v", err)
	}
	s.stream = nil
}

func (s *cmdStream) sendCommandMessage() error {
	var gCmd *pb.PCmdMessage

	if s.stream == nil {
		return status.Errorf(codes.Unavailable, "command stream is nil")
	}

	sKeys := make([]int32, 0)
	sKeys = append(sKeys, int32(pb.PCommandType_ECHO))
	sKeys = append(sKeys, int32(pb.PCommandType_ACTIVE_THREAD_COUNT))
	sKeys = append(sKeys, int32(pb.PCommandType_ACTIVE_THREAD_DUMP))
	sKeys = append(sKeys, int32(pb.PCommandType_ACTIVE_THREAD_LIGHT_DUMP))

	gCmd = &pb.PCmdMessage{
		Message: &pb.PCmdMessage_HandshakeMessage{
			HandshakeMessage: &pb.PCmdServiceHandshake{
				SupportCommandServiceKey: sKeys,
			},
		},
	}

	log("grpc").Debug("PCmdMessage: ", gCmd.String())

	return s.stream.Send(gCmd)
}

func (s *cmdStream) recvCommandRequest() error {
	var gCmdReq *pb.PCmdRequest

	if s.stream == nil {
		return status.Errorf(codes.Unavailable, "command stream is nil")
	}

	gCmdReq, err := s.stream.Recv()
	if err != nil {
		log("grpc").Errorf("fail to recv command request - %v", err)
		return err
	}

	log("grpc").Debug("PCmdRequest: ", gCmdReq.String())

	s.cmdReq = gCmdReq
	return nil
}

type activeThreadCountStream struct {
	stream   pb.ProfilerCommandService_CommandStreamActiveThreadCountClient
	reqId    int32
	actCount int32
}

func (cmdGrpc *cmdGrpc) newActiveThreadCountStream(reqId int32) *activeThreadCountStream {
	ctx := grpcMetadataContext(cmdGrpc.agent, -1)
	//ctx, _ = context.WithTimeout(ctx, 30 * time.Second)
	//defer cancel()

	stream, err := cmdGrpc.cmdClient.CommandStreamActiveThreadCount(ctx)
	if err != nil {
		log("grpc").Errorf("fail to make active thread count stream - %v", err)
		return &activeThreadCountStream{nil, -1, 0}
	}

	return &activeThreadCountStream{stream, reqId, 0}
}

func (s *activeThreadCountStream) close() {
	if s.stream == nil {
		return
	}

	err := s.stream.CloseSend()
	if err != nil {
		log("grpc").Errorf("fail to close command stream - %v", err)
	}
	s.stream = nil
}

func (s *activeThreadCountStream) sendActiveThreadCount() error {
	var gRes *pb.PCmdActiveThreadCountRes

	if s.stream == nil {
		return status.Errorf(codes.Unavailable, "command stream is nil")
	}

	now := time.Now()
	activeThreadCount := getActiveSpanCount(now)
	s.actCount++

	gRes = &pb.PCmdActiveThreadCountRes{
		CommonStreamResponse: &pb.PCmdStreamResponse{
			ResponseId: s.reqId,
			SequenceId: s.actCount,
			Message:    &wrappers.StringValue{Value: ""},
		},
		HistogramSchemaType: 2,
		ActiveThreadCount:   activeThreadCount,
		TimeStamp:           now.UnixNano() / int64(time.Millisecond),
	}

	log("grpc").Debug("PCmdActiveThreadCountRes: ", gRes.String())

	return s.stream.Send(gRes)
}

func (cmdGrpc *cmdGrpc) sendActiveThreadDump(reqId int32, limit int32, threadName []string, localId []int64, dump *GoroutineDump) {
	var gRes *pb.PCmdActiveThreadDumpRes

	gRes = &pb.PCmdActiveThreadDumpRes{
		CommonResponse: &pb.PCmdResponse{
			ResponseId: reqId,
			Status:     0,
			Message:    &wrappers.StringValue{Value: ""},
		},
		ThreadDump: makePActiveThreadDumpList(dump, int(limit), threadName, localId),
		Type:       "Go",
		SubType:    "",
		Version:    "1.14",
	}

	log("grpc").Debug("send PCmdActiveThreadDumpRes: ", gRes.String())

	ctx := grpcMetadataContext(cmdGrpc.agent, -1)
	_, err := cmdGrpc.cmdClient.CommandActiveThreadDump(ctx, gRes)
	if err != nil {
		log("grpc").Errorf("fail to CommandActiveThreadDump() - %v", err)
	}
}

func makePActiveThreadDumpList(dump *GoroutineDump, limit int, threadName []string, localId []int64) []*pb.PActiveThreadDump {
	dumpList := make([]*pb.PActiveThreadDump, 0)

	if limit < 1 {
		limit = len(dump.goroutines)
	}

	selected := make([]*Goroutine, 0)
	for _, tn := range threadName {
		g := dump.Search(tn)
		if g != nil {
			selected = append(selected, g)
		}
	}

	log("grpc").Debugf("send makePActiveThreadDumpList: %v", selected)

	for i := 0; i < limit && i < len(selected); i++ {
		aDump := makePActiveThreadDump(selected[i])
		dumpList = append(dumpList, aDump)
	}

	return dumpList
}

func makePActiveThreadDump(g *Goroutine) *pb.PActiveThreadDump {
	trace := make([]string, 0)
	trace = append(trace, g.trace)

	aDump := &pb.PActiveThreadDump{
		StartTime:    time.Now().UnixNano() / int64(time.Millisecond),
		LocalTraceId: 0,
		ThreadDump: &pb.PThreadDump{
			ThreadName:         g.header,
			ThreadId:           int64(g.id),
			BlockedTime:        0,
			BlockedCount:       0,
			WaitedTime:         0,
			WaitedCount:        0,
			LockName:           "",
			LockOwnerId:        0,
			LockOwnerName:      "",
			InNative:           false,
			Suspended:          false,
			ThreadState:        goRoutineState(g),
			StackTrace:         trace,
			LockedMonitor:      nil,
			LockedSynchronizer: nil,
		},
		Sampled:       false,
		TransactionId: "",
		EntryPoint:    "",
	}

	return aDump
}

func (cmdGrpc *cmdGrpc) sendActiveThreadLightDump(reqId int32, limit int32, dump *GoroutineDump) {
	var gRes *pb.PCmdActiveThreadLightDumpRes

	gRes = &pb.PCmdActiveThreadLightDumpRes{
		CommonResponse: &pb.PCmdResponse{
			ResponseId: reqId,
			Status:     0,                                //error
			Message:    &wrappers.StringValue{Value: ""}, //error message
		},
		ThreadDump: makePActiveThreadLightDumpList(dump, int(limit)),
		Type:       "Go",
		SubType:    "",
		Version:    "1.14", //go version
	}

	log("grpc").Debug("send PCmdActiveThreadLightDumpRes: ", gRes.String())

	ctx := grpcMetadataContext(cmdGrpc.agent, -1)
	_, err := cmdGrpc.cmdClient.CommandActiveThreadLightDump(ctx, gRes)
	if err != nil {
		log("grpc").Errorf("fail to make CommandActiveThreadLightDump() - %v", err)
	}
}

func makePActiveThreadLightDumpList(dump *GoroutineDump, limit int) []*pb.PActiveThreadLightDump {
	dumpList := make([]*pb.PActiveThreadLightDump, 0)

	if limit < 1 {
		limit = len(dump.goroutines)
	}

	for i := 0; i < limit && i < len(dump.goroutines); i++ {
		aDump := makePActiveThreadLightDump(dump.goroutines[i])
		dumpList = append(dumpList, aDump)
	}
	return dumpList
}

func makePActiveThreadLightDump(g *Goroutine) *pb.PActiveThreadLightDump {
	aDump := &pb.PActiveThreadLightDump{
		StartTime:    time.Now().UnixNano() / int64(time.Millisecond),
		LocalTraceId: 0,
		ThreadDump: &pb.PThreadLightDump{
			ThreadName:  g.header,
			ThreadId:    int64(g.id),
			ThreadState: goRoutineState(g),
		},
		Sampled:       false,
		TransactionId: "",
		EntryPoint:    "", //path
	}

	return aDump
}

func goRoutineState(g *Goroutine) pb.PThreadState {
	switch g.metas[MetaState] {
	case "running":
		return pb.PThreadState_THREAD_STATE_RUNNABLE
	case "select":
		return pb.PThreadState_THREAD_STATE_WAITING
	case "IO wait":
		return pb.PThreadState_THREAD_STATE_WAITING
	case "chan receive":
		return pb.PThreadState_THREAD_STATE_WAITING
	case "sleep":
		return pb.PThreadState_THREAD_STATE_BLOCKED
	default:
		break
	}

	return pb.PThreadState_THREAD_STATE_UNKNOWN
}

func (cmdGrpc *cmdGrpc) sendEcho(reqId int32, msg string) {
	var gRes *pb.PCmdEchoResponse

	gRes = &pb.PCmdEchoResponse{
		CommonResponse: &pb.PCmdResponse{
			ResponseId: reqId,
			Status:     0,                                //error
			Message:    &wrappers.StringValue{Value: ""}, //error message
		},
		Message: msg,
	}

	log("grpc").Debug("send PCmdEchoResponse: ", gRes.String())

	ctx := grpcMetadataContext(cmdGrpc.agent, -1)
	_, err := cmdGrpc.cmdClient.CommandEcho(ctx, gRes)
	if err != nil {
		log("grpc").Errorf("fail to CommandEcho() - %v", err)
	}
}
