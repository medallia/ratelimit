package ratelimit

import (
	"strconv"
	"strings"
	"sync"

	pb "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	"github.com/envoyproxy/ratelimit/src/assert"
	"github.com/envoyproxy/ratelimit/src/config"
	"github.com/envoyproxy/ratelimit/src/limiter"
	"github.com/envoyproxy/ratelimit/src/redis"
	"github.com/lyft/goruntime/loader"
	stats "github.com/lyft/gostats"
	logger "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
)

type shouldRateLimitStats struct {
	redisError   stats.Counter
	serviceError stats.Counter
}

func newShouldRateLimitStats(scope stats.Scope) shouldRateLimitStats {
	ret := shouldRateLimitStats{}
	ret.redisError = scope.NewCounter("redis_error")
	ret.serviceError = scope.NewCounter("service_error")
	return ret
}

type serviceStats struct {
	configLoadSuccess stats.Counter
	configLoadError   stats.Counter
	shouldRateLimit   shouldRateLimitStats
}

func newServiceStats(scope stats.Scope) serviceStats {
	ret := serviceStats{}
	ret.configLoadSuccess = scope.NewCounter("config_load_success")
	ret.configLoadError = scope.NewCounter("config_load_error")
	ret.shouldRateLimit = newShouldRateLimitStats(scope.Scope("call.should_rate_limit"))
	return ret
}

type RateLimitServiceServer interface {
	pb.RateLimitServiceServer
	GetCurrentConfig() config.RateLimitConfig
	GetLegacyService() RateLimitLegacyServiceServer
}

type service struct {
	runtime            loader.IFace
	configLock         sync.RWMutex
	configLoader       config.RateLimitConfigLoader
	config             config.RateLimitConfig
	runtimeUpdateEvent chan int
	cache              limiter.RateLimitCache
	stats              serviceStats
	rlStatsScope       stats.Scope
	legacy             *legacyService
	runtimeWatchRoot   bool
}

func (this *service) reloadConfig() {
	defer func() {
		if e := recover(); e != nil {
			configError, ok := e.(config.RateLimitConfigError)
			if !ok {
				panic(e)
			}

			this.stats.configLoadError.Inc()
			logger.Errorf("error loading new configuration from runtime: %s", configError.Error())
		}
	}()

	files := []config.RateLimitConfigToLoad{}
	snapshot := this.runtime.Snapshot()
	for _, key := range snapshot.Keys() {
		if this.runtimeWatchRoot && !strings.HasPrefix(key, "config.") {
			continue
		}

		files = append(files, config.RateLimitConfigToLoad{key, snapshot.Get(key)})
	}

	newConfig := this.configLoader.Load(files, this.rlStatsScope)
	this.stats.configLoadSuccess.Inc()
	this.configLock.Lock()
	this.config = newConfig
	this.configLock.Unlock()
}

type serviceError string

func (e serviceError) Error() string {
	return string(e)
}

func checkServiceErr(something bool, msg string) {
	if !something {
		panic(serviceError(msg))
	}
}

func populateCustomHitsAddend(request *pb.RateLimitRequest) error {
	// TODO if enabled in config
	for i := 0; i < len(request.Descriptors); i++ {
		// TODO get descriptor key from config. Ideally per domain, but maybe a global configuration is good enough too.
		if len(request.Descriptors[i].Entries) == 1 && request.Descriptors[i].Entries[0].Key == "customHitsKey" {
			// TODO error handling
			hitsAddend, err := strconv.ParseUint(request.Descriptors[i].Entries[0].Value, 10, 32)
			if err != nil {
				return err
			}

			request.HitsAddend = uint32(hitsAddend)

			logger.Debugf("Custom hits addend %d", request.HitsAddend)

			// Remove hitsAddendDescriptor
			d := request.Descriptors
			copy(d[i:], d[i+1:])
			request.Descriptors = d[:len(d)-1]
			break
		}
	}

	return nil
}

func (this *service) shouldRateLimitWorker(
	ctx context.Context, request *pb.RateLimitRequest) *pb.RateLimitResponse {

	checkServiceErr(request.Domain != "", "rate limit domain must not be empty")
	checkServiceErr(len(request.Descriptors) != 0, "rate limit descriptor list must not be empty")

	snappedConfig := this.GetCurrentConfig()
	checkServiceErr(snappedConfig != nil, "no rate limit configuration loaded")

	checkServiceErr(populateCustomHitsAddend(request) == nil, "customHitsKey must be a positive integer")

	limitsToCheck := make([]*config.RateLimit, len(request.Descriptors))
	for i, descriptor := range request.Descriptors {
		limitsToCheck[i] = snappedConfig.GetLimit(ctx, request.Domain, descriptor)
	}

	responseDescriptorStatuses := this.cache.DoLimit(ctx, request, limitsToCheck)
	logger.Warnf("LL %d DL %d", len(limitsToCheck), len(responseDescriptorStatuses))
	assert.Assert(len(limitsToCheck) == len(responseDescriptorStatuses))

	response := &pb.RateLimitResponse{}
	response.Statuses = make([]*pb.RateLimitResponse_DescriptorStatus, len(request.Descriptors))
	finalCode := pb.RateLimitResponse_OK
	for i, descriptorStatus := range responseDescriptorStatuses {
		response.Statuses[i] = descriptorStatus
		if descriptorStatus.Code == pb.RateLimitResponse_OVER_LIMIT {
			finalCode = descriptorStatus.Code
		}
	}

	response.OverallCode = finalCode
	return response
}

func (this *service) ShouldRateLimit(
	ctx context.Context,
	request *pb.RateLimitRequest) (finalResponse *pb.RateLimitResponse, finalError error) {

	defer func() {
		err := recover()
		if err == nil {
			return
		}

		logger.Debugf("caught error during call")
		finalResponse = nil
		switch t := err.(type) {
		case redis.RedisError:
			{
				this.stats.shouldRateLimit.redisError.Inc()
				finalError = t
			}
		case serviceError:
			{
				this.stats.shouldRateLimit.serviceError.Inc()
				finalError = t
			}
		default:
			panic(err)
		}
	}()

	response := this.shouldRateLimitWorker(ctx, request)
	logger.Debugf("returning normal response")
	return response, nil
}

func (this *service) GetLegacyService() RateLimitLegacyServiceServer {
	return this.legacy
}

func (this *service) GetCurrentConfig() config.RateLimitConfig {
	this.configLock.RLock()
	defer this.configLock.RUnlock()
	return this.config
}

func NewService(runtime loader.IFace, cache limiter.RateLimitCache,
	configLoader config.RateLimitConfigLoader, stats stats.Scope, runtimeWatchRoot bool) RateLimitServiceServer {

	newService := &service{
		runtime:            runtime,
		configLock:         sync.RWMutex{},
		configLoader:       configLoader,
		config:             nil,
		runtimeUpdateEvent: make(chan int),
		cache:              cache,
		stats:              newServiceStats(stats),
		rlStatsScope:       stats.Scope("rate_limit"),
		runtimeWatchRoot:   runtimeWatchRoot,
	}
	newService.legacy = &legacyService{
		s:                          newService,
		shouldRateLimitLegacyStats: newShouldRateLimitLegacyStats(stats),
	}

	runtime.AddUpdateCallback(newService.runtimeUpdateEvent)

	newService.reloadConfig()
	go func() {
		// No exit right now.
		for {
			logger.Debugf("waiting for runtime update")
			<-newService.runtimeUpdateEvent
			logger.Debugf("got runtime update and reloading config")
			newService.reloadConfig()
		}
	}()

	return newService
}
