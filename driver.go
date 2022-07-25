package corral

import (
	"context"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ISE-SMILE/corral/compute/polling"

	"github.com/ISE-SMILE/corral/api"
	"github.com/ISE-SMILE/corral/internal/corcache"
	"github.com/ISE-SMILE/corral/internal/corfs"

	"github.com/dustin/go-humanize"

	"github.com/spf13/viper"

	"golang.org/x/sync/semaphore"

	log "github.com/sirupsen/logrus"
	pb "gopkg.in/cheggaaa/pb.v1"

	flag "github.com/spf13/pflag"
)

var src rand.Source

func init() {
	rand.Seed(time.Now().UnixNano())
	src = rand.NewSource(time.Now().UnixNano())
}

// Driver controls the execution of a MapReduce Job
type Driver struct {
	jobs      []*Job
	config    *config
	executor  executor
	cache     api.CacheSystem
	polling   api.PollingStrategy
	runtimeID string
	Start     time.Time

	currentJob int
	Runtime    time.Duration

	lastOutputs []string
}

func (d *Driver) CurrentJob() *Job {
	if d.currentJob >= 0 && d.currentJob < len(d.jobs) {
		return d.jobs[d.currentJob]
	}
	return nil
}

// config configures a Driver's execution of jobs
type config struct {
	Inputs          []string
	StageInputs     [][]string
	SplitSize       int64
	MapBinSize      int64
	ReduceBinSize   int64
	MaxConcurrency  int
	WorkingLocation string
	Cleanup         bool
	Cache           api.CacheSystemType
	Backend         string
	Polling         api.PollingStrategy
}

func newConfig() *config {
	loadConfig() // Load viper config from settings file(s) and environment

	// Register command line flags
	flag.Parse()
	viper.BindPFlags(flag.CommandLine)

	return &config{
		Inputs:          []string{},
		SplitSize:       viper.GetInt64("splitSize"),
		MapBinSize:      viper.GetInt64("mapBinSize"),
		ReduceBinSize:   viper.GetInt64("reduceBinSize"),
		MaxConcurrency:  viper.GetInt("maxConcurrency"),
		WorkingLocation: viper.GetString("workingLocation"),
		Cleanup:         viper.GetBool("cleanup"),
	}
}

// Option allows configuration of a Driver
type Option func(*config)

// NewDriver creates a new Driver with the provided job and optional configuration
func NewDriver(job *Job, options ...Option) *Driver {
	d := &Driver{
		jobs:      []*Job{job},
		executor:  &localExecutor{time.Now()},
		runtimeID: randomName(),
		Start:     time.Now(),
		polling:   &polling.BackoffPolling{},
	}

	c := newConfig()
	for _, f := range options {
		f(c)
	}

	if c.SplitSize > c.MapBinSize {
		log.Warn("Configured Split Size is larger than Map Bin size")
		c.SplitSize = c.MapBinSize
	}

	if c.SplitSize == 0 || c.MapBinSize == 0 || c.ReduceBinSize == 0 {
		panic(fmt.Errorf("split sizes can't be 0 split:%d,map:%d,reduce:%d", c.SplitSize, c.MapBinSize, c.ReduceBinSize))
	}

	if c.Polling != nil {
		d.polling = c.Polling
	}

	d.config = c
	log.Debugf("Loaded config: %#v", c)

	if c.Cache != api.NoCache {
		cache, err := corcache.NewCacheSystem(c.Cache)
		if err != nil {
			log.Debugf("failed to init cache, %+v", err)
			panic(err)
		} else {
			log.Infof("using cache %+v", c.Cache)
		}
		d.cache = cache
	}

	return d
}

// NewSequentialMultiStageDriver creates a new Driver with the provided jobs and optional configuration. Jobs are executed in sequence with each output becoming the input of the next stage.
func NewSequentialMultiStageDriver(jobs []*Job, options ...Option) *Driver {
	driver := NewDriver(nil, options...)
	driver.jobs = jobs

	if driver.config.StageInputs != nil {
		//overwride possible WithInputs options
		driver.config.Inputs = driver.config.StageInputs[0]
	}

	return driver
}

// WithSplitSize sets the SplitSize of the Driver
func WithSplitSize(s int64) Option {
	return func(c *config) {
		c.SplitSize = s
	}
}

// WithMapBinSize sets the MapBinSize of the Driver
func WithMapBinSize(s int64) Option {
	return func(c *config) {
		c.MapBinSize = s
	}
}

// WithReduceBinSize sets the ReduceBinSize of the Driver
func WithReduceBinSize(s int64) Option {
	return func(c *config) {
		c.ReduceBinSize = s
	}
}

//WithMultipleSize adjusts the splitsize by a multiple. Warn! uses math.Ceil to "round", set the sizes manually if u need precision.
func WithMultipleSize(mul float64) Option {
	return func(c *config) {
		c.ReduceBinSize = int64(math.Ceil(float64(c.ReduceBinSize) * mul))
		c.MapBinSize = int64(math.Ceil(float64(c.MapBinSize) * mul))
		c.SplitSize = int64(math.Ceil(float64(c.SplitSize) * mul))

		log.Debugf("using size reduce:%dMB map:%dMB split:%dMB", c.ReduceBinSize/1024/1024, c.MapBinSize/1024/1024, c.SplitSize/1024/1024)
	}
}

// WithWorkingLocation sets the location and filesystem backend of the Driver
func WithWorkingLocation(location string) Option {
	return func(c *config) {
		c.WorkingLocation = location
	}
}

// WithInputs specifies job inputs (i.e. input files/directories)
func WithInputs(inputs ...string) Option {
	return func(c *config) {
		c.Inputs = append(c.Inputs, inputs...)
	}
}

// WithMultiStageInputs specifies job inputs (i.e. input files/directories) for each stage
func WithMultiStageInputs(inputs [][]string) Option {
	return func(c *config) {
		c.StageInputs = inputs
	}
}

func WithLocalMemoryCache() Option {
	return func(c *config) {
		c.Cache = api.InMemory
	}
}

func WithRedisBackedCache() Option {
	return func(c *config) {
		c.Cache = api.Redis
	}
}

func WithLambdaRole(arn string) Option {
	return func(c *config) {
		viper.Set("lambdaManageRole", false)
		viper.Set("lambdaRoleARN", arn)
	}
}

func WithLambdaS3Backend(bucket, key string) Option {
	return func(c *config) {
		viper.Set("lambdaS3Bucket", bucket)
		viper.Set("lambdaS3Key", key)
	}
}

func WithBackoffPolling() Option {
	return func(c *config) {
		c.Polling = &polling.BackoffPolling{}
	}
}

func (d *Driver) GetFinalOutputs() []string {
	return d.lastOutputs
}

func (d *Driver) DownloadAndRemove(inputs []string, dest string) error {
	fs := corfs.InferFilesystem(inputs[0])

	files := make([]string, 0)
	for _, input := range inputs {
		list, err := fs.ListFiles(input)
		if err != nil {
			return err
		}
		for _, f := range list {
			files = append(files, f.Name)
		}

	}

	log.Infof("found %d files to download", len(files))
	bar := pb.New(len(files)).Prefix("DownloadAndRemove").Start()
	for _, path := range files {

		dst := filepath.Join(dest, filepath.Base(path))
		wr, err := os.OpenFile(dst, os.O_CREATE|os.O_RDWR, 0664)
		if err != nil {
			return err
		}
		r, err := fs.OpenReader(path, 0)
		if err != nil {
			return err
		}
		_, err = io.Copy(wr, r)
		if err != nil {
			return err
		}

		err = fs.Delete(path)
		if err != nil {
			return err
		}

		bar.Increment()
	}
	bar.Finish()

	return nil
}

func (d *Driver) runMapPhase(job *Job, jobNumber int, inputs []string) {
	inputSplits := job.inputSplits(inputs, d.config.SplitSize)
	if len(inputSplits) == 0 {
		log.Warnf("No input splits")
		return
	}
	log.Debugf("Number of job input splits: %d", len(inputSplits))

	inputBins := packInputSplits(inputSplits, d.config.MapBinSize)
	go d.collectMapMetrics(jobNumber, inputBins)
	log.Debugf("Number of job input bins: %d", len(inputBins))
	bar := pb.New(len(inputBins)).Prefix("Map").Start()

	//tell the platfrom how may invocations we plan on dooing
	if viper.GetBool("hinting") {
		err := d.executor.HintSplits(uint(len(inputBins)))
		if err != nil {
			log.Warn("failed to hint platfrom, expect perfromance degredations")
			log.Debugf("hint error:%+v", err)
		}
	}

	if viper.GetBool("eventBatching") {
		sem := d.executor.(smileExecutor)
		for i := 0; i < 3; i++ {
			err := sem.BatchRunMapper(job, jobNumber, inputBins)
			if err != nil {
				log.Errorf("Error when running batch mapper %s", err)
			} else {
				break
			}
		}

	} else {
		var wg sync.WaitGroup
		sem := semaphore.NewWeighted(int64(d.config.MaxConcurrency))
		for binID, bin := range inputBins {
			//XXX: binID casted to uint
			sem.Acquire(context.Background(), 1)
			wg.Add(1)
			go func(bID uint, b []api.InputSplit) {
				defer wg.Done()
				defer sem.Release(1)
				defer bar.Increment()
				err := d.executor.RunMapper(job, jobNumber, bID, b)
				if err != nil {
					log.Errorf("Error when running mapper %d: %s", bID, err)
				}
			}(uint(binID), bin)
		}
		wg.Wait()
	}
	bar.Finish()
}

func (d *Driver) runReducePhase(job *Job, jobNumber int) {
	var wg sync.WaitGroup
	bar := pb.New(int(job.intermediateBins)).Prefix("Reduce").Start()

	//tell the platfrom how may invocations we plan on dooing
	if viper.GetBool("hinting") {
		err := d.executor.HintSplits(job.intermediateBins)
		if err != nil {
			log.Warn("failed to hint platfrom, expect perfromance degredations")
			log.Debugf("hint error:%+v", err)
		}
	}
	if viper.GetBool("eventBatching") {
		sem := d.executor.(smileExecutor)
		bins := make([]uint, job.intermediateBins)
		for i := 0; i < len(bins); i++ {
			bins[i] = uint(i)
		}
		for i := 0; i < 3; i++ {
			err := sem.BatchRunReducer(job, jobNumber, bins)
			if err != nil {
				log.Errorf("Error when running batch mapper %s", err)
			} else {
				break
			}
		}
	} else {
		for binID := uint(0); binID < job.intermediateBins; binID++ {
			wg.Add(1)
			go func(bID uint) {
				defer wg.Done()
				defer bar.Increment()
				err := d.executor.RunReducer(job, jobNumber, bID)
				if err != nil {
					log.Errorf("Error when running reducer %d: %s", bID, err)
				}
			}(binID)
		}
		wg.Wait()
	}
	bar.Finish()
}

func RunningOnCloudPlatfrom() bool {
	return runningInLambda() || runningInWhisk()
}

func (d *Driver) runOnCloudPlatfrom() bool {
	if runningInLambda() {
		log.Debug(">>>Running on AWS Lambda>>>")
		//XXX this is sub-optimal (in case we need to init this struct we have to come up with a different strategy ;)
		(&lambdaExecutor{}).Start(d)
		return true
	}
	if runningInWhisk() {
		log.Debug(">>>Running on OpenWhisk or IBM>>>")
		(&whiskExecutor{}).Start(d)
		return true
	}

	return false
}

// run starts the Driver
func (d *Driver) run() {
	if d.runOnCloudPlatfrom() {
		log.Warn("Running on FaaS runtime and Returned, this is bad!")
		fmt.Println("Function Loop Termintated!")
		os.Exit(-10)
	}
	if !validateFlagConfig(d) {
		panic(fmt.Errorf("failed to validate flags, can't execute"))
	}

	if d.cache != nil {
		err := d.cache.Check()
		if err != nil {
			log.Errorf("Cache failed pre-flight checks %+v", err)
			return
		}
		err = d.cache.Deploy()
		if err != nil {
			log.Errorf("failed to deploy cache, %+v", err)
			return
		}

		err = d.cache.Init()
		if err != nil {
			log.Errorf("failed to initilized cache, %+v", err)
			return
		}
	}

	//TODO: do preflight checks e.g. check if in/out is accessible...
	//TODO introduce interface for deploy/undeploy
	if lBackend, ok := d.executor.(platform); ok {
		err := lBackend.Deploy(d)
		if err != nil {
			panic(err)
		}
	}

	if len(d.config.Inputs) == 0 {
		log.Error("No inputs!")
		log.Error(os.Environ())
		return
	}

	if strings.Contains(d.config.Inputs[0], "://") {
		//We are using remote inputs
		if backendFlag != nil && *backendFlag != "local" {
			//we are runing remotely?
			if !strings.Contains(d.config.WorkingLocation, "://") {
				log.Errorf("Running remote executer without remote output location. set --out to a reachable output location for the executor.")
				return
			}
		}
	}

	if rlh := viper.GetString("remoteLoggingHost"); rlh != "" {
		log.Debugf("using remote logging host: %s for functions", rlh)
	}

	var inputs []string
	if d.config.StageInputs != nil {
		//we need to avoid adding inputs twice ;)
		inputs = make([]string, 0)
	} else {
		inputs = d.config.Inputs
	}

	for idx, job := range d.jobs {
		if viper.GetBool("verbose") || *verbose {
			log.Debugf("collecting job metrics")
			go job.CollectMetrics()
		}
		if d.config.StageInputs != nil {
			//adding stage inputs
			inputs = append(inputs, d.config.StageInputs[idx]...)
		}

		// Initialize job filesystem
		job.fileSystem = corfs.InferFilesystem(inputs[0])

		//set cache system based on driver
		job.cacheSystem = d.cache

		jobWorkingLoc := d.config.WorkingLocation
		log.Infof("Starting job%d (%d/%d)", idx, idx+1, len(d.jobs))

		if len(d.jobs) > 1 {
			jobWorkingLoc = job.fileSystem.Join(jobWorkingLoc, fmt.Sprintf("job%d", idx))
		}
		job.outputPath = jobWorkingLoc

		*job.config = *d.config

		err := d.polling.StartJob(api.JobInfo{
			JobId:          idx,
			Splits:         len(inputs),
			SplitSize:      d.config.SplitSize,
			MapBinSize:     d.config.MapBinSize,
			ReduceBinSize:  d.config.ReduceBinSize,
			MaxConcurrency: d.config.MaxConcurrency,
			Backend:        d.config.Backend,
			FunctionMemory: viper.GetInt("lambdaMemory"),
			CacheType:      viper.GetInt("cache"),
			MapLOC:         0, //TODO
			ReduceLoc:      0, //TODO
		})
		if err != nil {
			log.Debugf("failed to init polling %+e", err)
			panic(err)
		}

		d.runMapPhase(job, idx, inputs)

		go d.collectReducerMetrics(idx, job)

		d.runReducePhase(job, idx)

		// Set inputs of next job to be outputs of current job
		inputs = []string{job.fileSystem.Join(jobWorkingLoc, "output-*")}
		d.lastOutputs = inputs

		log.Infof("Job %d - Total Bytes Read:\t%s", idx, humanize.Bytes(uint64(job.bytesRead)))
		log.Infof("Job %d - Total Bytes Written:\t%s", idx, humanize.Bytes(uint64(job.bytesWritten)))

		//check if we need to flush the intermedate data to disk
		if viper.GetBool("durable") && !viper.GetBool("cleanup") {
			if d.cache != nil {
				//we dont need to wait for this to finish, this might also take a while ...
				go func(cache api.CacheSystem, fs api.FileSystem) {
					err := cache.Flush(fs)

					if err != nil {
						log.Errorf("failed to flush cache to fs, %+v", err)
					}
				}(d.cache, job.fileSystem)
			}
		}

		//clear cache
		if viper.GetBool("cleanup") && d.cache != nil {
			err := d.cache.Clear()
			if err != nil {
				log.Warnf("failed to cleanup cache, %+v", err)
			}
		}

		job.done()
	}
}

func (d *Driver) collectMapMetrics(jId int, inputs [][]api.InputSplit) {
	if !viper.GetBool("collectInputSizes") {
		return
	}

	binSizes := make(map[int]int64)
	for bin, splits := range inputs {
		for _, split := range splits {
			binSizes[bin] += split.Size()
		}
	}
	d.polling.UpdateJob(api.JobInfo{
		JobId:       jId,
		MapBinSizes: binSizes,
	})
}

func (d *Driver) collectReducerMetrics(jId int, j *Job) {
	if !viper.GetBool("collectInputSizes") {
		return
	}

	var fs api.FileSystem = j.fileSystem
	if j.cacheSystem != nil {
		fs = j.cacheSystem
	}
	// Determine the intermediate data files this reducer is responsible for
	path := fs.Join(j.outputPath, "map-bin*")
	files, err := fs.ListFiles(path)
	if err != nil {
		log.Debugf("failed to collect reduce metrics cause: %+v", err)
	}

	binSizes := make(map[int]int64)
	for _, file := range files {
		names := fs.Split(file.Name)

		//("map-bin<binIdx>-<mapper>.out"
		binStr := strings.Split(names[len(names)-1], "-")[1][3:]
		binIdx, err := strconv.Atoi(binStr)
		if err == nil {
			binSizes[binIdx] += file.Size
		}
	}
	d.polling.UpdateJob(api.JobInfo{
		JobId:          jId,
		ReduceBinSizes: binSizes,
	})
}

var backendFlag = flag.StringP("backend", "b", "", "Define backend [local,lambda,whisk] - default local")

var outputDir = flag.StringP("out", "o", "", "Output `directory` (can be local or in S3)")
var memprofile = flag.String("memprofile", "", "Write memory profile to `file`")
var verbose = flag.BoolP("verbose", "v", false, "Output verbose logs")
var undeploy = flag.Bool("undeploy", false, "Undeploy the Lambda function and IAM permissions without running the driver")

// Main starts the Driver, running the submitted jobs.
func (d *Driver) Main() {
	defer api.StopAllRunningPlugins()

	if viper.GetBool("verbose") || *verbose {
		log.SetLevel(log.DebugLevel)
	}
	if *undeploy {
		//TODO: this is a shitty interface/abstraction
		err := d.Undeploy(backendFlag)
		if err != nil {
			log.Infof("undeploy failed with %+v", err)
		}
		return
	}

	d.config.Inputs = append(d.config.Inputs, flag.Args()...)
	d.WithBackend(backendFlag)

	if *outputDir != "" {
		d.config.WorkingLocation = *outputDir
	}

	d.Execute()
	if *memprofile != "" {
		f, err := os.Create(*memprofile)
		if err != nil {
			log.Fatal("could not create memory profile: ", err)
		}
		runtime.GC() // get up-to-date statistics
		if err := pprof.WriteHeapProfile(f); err != nil {
			log.Fatal("could not write memory profile: ", err)
		}
		f.Close()
	}

}

func (d *Driver) Execute() {
	log.Debugf("Using Config %+v", viper.AllSettings())
	log.Debugf("Mode [%s] - %s", CompileFlagName(), d.config.Backend)
	start := time.Now()
	d.run()
	end := time.Now()
	fmt.Printf("Job Execution Time: %s\n", end.Sub(start))
	d.Runtime = end.Sub(start)
}

func (d *Driver) WithBackend(backendType *string) {
	if backendType != nil {
		if *backendType == "lambda" {
			d.executor = newLambdaExecutor(viper.GetString("lambdaFunctionName"))
		} else if *backendType == "whisk" {
			d.executor = newWhiskExecutor(viper.GetString("lambdaFunctionName"), d.polling)
		} else {
			log.Warnf("unknowen backend flag %+v", *backendType)
		}

		if d.config != nil {
			d.config.Backend = *backendType
		}
	}
}

func (d *Driver) Undeploy(backendType *string) error {
	if d.cache != nil {
		err := d.cache.Undeploy()
		if err != nil {
			log.Warnf("failed to undeploy cache, you are on youre own %s", err)
			return err
		}
	}

	if backendType == nil {
		panic("missing backend flag!")
	}
	//TODO: modify the constants
	var backend platform
	if *backendType == "lambda" {
		backend = newLambdaExecutor(viper.GetString("lambdaFunctionName"))
	} else if *backendType == "whisk" {
		backend = newWhiskExecutor(viper.GetString("lambdaFunctionName"), nil)
	} else {
		return fmt.Errorf("unkown backend flag")
	}

	err := backend.Undeploy()
	if err != nil {
		log.Errorf("failed to undeploy, you are on youre own %s", err)
		return err
	}

	return nil
}
