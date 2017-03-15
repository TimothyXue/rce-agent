// Copyright 2017 Square, Inc.

package rce

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v2"

	"google.golang.org/grpc"
	log "google.golang.org/grpc/grpclog"

	pb "github.com/square/rce-agent/pb"
	"golang.org/x/net/context"
)

const (
	STATUS_NOT_STARTED int64 = iota
	STATUS_RUNNING
	STATUS_COMPLETED
)

// Interface for the RCE Agent Server
type Server interface {

	// Get all jobs after a specific start point
	GetJobs(since *pb.StartTime, stream pb.RCEAgent_GetJobsServer) error

	// Get the status of the job with the specified id
	GetJobStatus(ctx context.Context, id *pb.JobID) (*pb.JobStatus, error)

	// Start a new job. This is a non-blocking call.
	StartJob(ctx context.Context, details *pb.JobRequest) (*pb.JobStatus, error)
	// TODO: new rpc fn to stream output?
	// TODO: enable a job to be run with sudo

	// Given a job ID, stop that job.
	StopJob(ctx context.Context, id *pb.JobID) (*pb.JobStatus, error)

	// Start the server. This is a non-blocking call.
	Start() error

	// Stop the server.
	Stop() error
}

// non-exported struct
type server struct {
	allJobs          map[uint64]*runningJob // Map of all previously requested jobs. map[jobID] -> job
	jobsM            *sync.RWMutex          // Lock for allJobs
	runnableCommands map[string]string      // Map of runnable commands for this agent: map[command name] -> command
	nextID           uint64                 // the next available JobID
	IDm              *sync.Mutex            // Mutex for nextID

	// server stuff
	laddr      string       // host:port listen address
	grpcServer *grpc.Server // gRPC server instance that this agent is using
}

type runningJob struct {
	Status  *pb.JobStatus // The actual status of the job
	stdoutM *sync.Mutex   // Mutex for locking changes to the job status
	stderrM *sync.Mutex   // Mutex for locking changes to the stderr log
	statusM *sync.Mutex   // Mutex for locking changes to the stdout log
}

// Makes a copy of the job status for rj.
// Useful for returning a snapshot of the current job state.
func (rj *runningJob) CopyStatus() *pb.JobStatus {
	rj.statusM.Lock()
	rj.stdoutM.Lock()
	rj.stderrM.Lock()
	jc := &pb.JobStatus{}
	*jc = *(rj.Status)
	rj.stderrM.Unlock()
	rj.stdoutM.Unlock()
	rj.statusM.Unlock()
	return jc
}

// NewServer creates a new gRPC server listening on the given host:port (laddr)
// and using the given configFile to load allowed commands. The server is not
// started.
func NewServer(laddr string, configFile string) (Server, error) {

	cfg, err := LoadRunnableCommands(configFile)
	if err != nil {
		return nil, err
	}
	s := &server{
		jobsM:            &sync.RWMutex{},
		allJobs:          map[uint64]*runningJob{},
		nextID:           0,
		runnableCommands: cfg.Commands,
		IDm:              &sync.Mutex{},
		laddr:            laddr,
	}
	grpcServer := grpc.NewServer()
	pb.RegisterRCEAgentServer(grpcServer, s)
	// TODO: use tls

	s.grpcServer = grpcServer
	return s, nil
}

// Config struct  for parsing the input config yaml file
type Config struct {
	Commands map[string]string `yaml:"commands"` // This is the whitelist of available commands
	TLS      struct {          // SSL Configuration settings
		sslCAFile  string `yaml:"sslCAFile"`  // SSL CA filepath
		sslCrtFile string `yaml:"sslCrtFile"` // SSL CRT filepath
		sslKeyFile string `yaml:"sslKeyFile"` // SSL Key Filepath
	} `yaml:"tlsconfig"` // TODO:  ssl isnt even tested yet
}

// Loads the config file into memory.
func LoadRunnableCommands(configFile string) (*Config, error) {
	f, err := ioutil.ReadFile(configFile)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	err = yaml.Unmarshal(f, cfg)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

// Starts the gRPC server. This function is non blocking and will
// return an error if there is one starting the listener.
// It is up to the user to call s.Stop() to properly stop the
// server.
func (s *server) Start() error {
	lis, err := net.Listen("tcp", s.laddr)
	if err != nil {
		return err
	}
	go s.grpcServer.Serve(lis)
	return nil
}

// Stops the gRPC server.
func (s *server) Stop() error {
	s.grpcServer.GracefulStop()
	return nil
}

// GetJobs list all jobs that have started after since.
func (s *server) GetJobs(since *pb.StartTime, stream pb.RCEAgent_GetJobsServer) error {
	log.Println("Getting all jobs.")
	s.jobsM.RLock()
	defer s.jobsM.RUnlock()
	return nil
	for id, job := range s.allJobs {
		if job.Status.StartTime > since.StartTime {
			j := &pb.JobID{JobID: id}
			if err := stream.Send(j); err != nil {
				return err
			}
		}
	}
	return nil
}

// GetJobStatus returns the job status of the given job id.
func (s *server) GetJobStatus(ctx context.Context, id *pb.JobID) (*pb.JobStatus, error) {
	log.Printf("job=%d: Status request received.", id.JobID)
	return s.getJobStatus(id.JobID)
}

// getJobStatus returns the job status of the given job id.
func (s *server) getJobStatus(id uint64) (*pb.JobStatus, error) {
	s.jobsM.RLock()
	job, ok := s.allJobs[id]
	s.jobsM.RUnlock()
	if !ok {
		return nil, fmt.Errorf("job=%d: Job not found.", id)
	}

	jc := job.CopyStatus()
	return jc, nil
}

// Stops an already running job by sending the process a SIGTERM signal.
// If the job does not exist, or is not running, then an error is returned.
func (s *server) StopJob(ctx context.Context, id *pb.JobID) (*pb.JobStatus, error) {
	log.Printf("job=%d: Stop request received.", id.JobID)
	s.jobsM.RLock()
	job, ok := s.allJobs[id.JobID]
	s.jobsM.RUnlock()
	if !ok {
		return nil, fmt.Errorf("job=%d: Job not found.", id.JobID)
	}

	if job.Status.Status != STATUS_RUNNING {
		return nil, fmt.Errorf("job=%d: Job not running.", id.JobID)
	}

	// TODO: how would we test this?
	proc, err := os.FindProcess(int(job.Status.PID))
	if err != nil {
		return nil, fmt.Errorf("job=%d: Process with pid %d not found: %v", id.JobID, job.Status.PID, err)
	}

	// TODO: figure out the correct signal to send
	err = proc.Signal(syscall.SIGTERM)
	if err != nil {
		return nil, fmt.Errorf("job=%d: Error killing pid %d: %v", id.JobID, job.Status.PID, err)
	}
	// TODO: need to wait until the job goroutine finishes updating status?

	log.Printf("job=%d: Successfully killed pid %d", id.JobID, job.Status.PID)
	return s.getJobStatus(id.JobID)
}

// Starts a job. This will start the job in another goroutine and will immediately
// return the status for that job. It is up to the client to pull for the job status
// to determine completion/output.
func (s *server) StartJob(ctx context.Context, details *pb.JobRequest) (*pb.JobStatus, error) {
	newJobStatus := &pb.JobStatus{
		JobID:       s.getNewJobID(),
		JobName:     details.JobName,
		Status:      STATUS_NOT_STARTED,
		CommandName: details.CommandName,
		StartTime:   time.Now().Unix(),
		ExitCode:    -1,
		Args:        details.Arguments,
	}
	log.Printf("job=%d: New job received (name: %s, commandName: %s, args: \"%s\").", newJobStatus.JobID,
		newJobStatus.JobName, newJobStatus.CommandName, strings.Join(newJobStatus.Args, " "))

	newJob := &runningJob{
		Status:  newJobStatus,
		stdoutM: &sync.Mutex{},
		stderrM: &sync.Mutex{},
		statusM: &sync.Mutex{},
	}

	s.jobsM.Lock()
	s.allJobs[newJob.Status.JobID] = newJob
	s.jobsM.Unlock()

	go s.runJob(newJob)

	jc := newJob.CopyStatus()
	return jc, nil
}

// Reads from the provided ReadCloser, rc, and copies lines into the
// dest string array. Locks dest with the provided mutex
func copyPipeToStringArray(rc io.ReadCloser, dest *[]string, m *sync.Mutex) {
	defer rc.Close()
	r := bufio.NewReader(rc)
	s, err := r.ReadString('\n')
	for err == nil {
		m.Lock()
		*dest = append(*dest, strings.TrimSpace(s))
		m.Unlock()
		s, err = r.ReadString('\n')
	}
}

// TODO: refactor this to be more testable
func (s *server) runJob(job *runningJob) {
	log.Printf("job=%d: Running job.", job.Status.JobID)
	commandToExecute, ok := s.runnableCommands[job.Status.CommandName]
	if !ok {
		//TODO: make a function for creating errors like these
		job.statusM.Lock()
		job.Status.Status = STATUS_COMPLETED
		job.Status.Error = "Unable to find job for " + job.Status.CommandName
		job.statusM.Unlock()
		return
	}

	////////////////////////////////////////////////////////
	// Setup Command                                      //
	////////////////////////////////////////////////////////

	// Build command
	cmd := exec.Command(commandToExecute, job.Status.Args...)

	// Pipe stdout to collect in jobstatus
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		job.statusM.Lock()
		job.Status.Status = STATUS_COMPLETED
		job.Status.Error = err.Error()
		job.statusM.Unlock()
		return
	}
	go copyPipeToStringArray(stdoutPipe, &job.Status.Stdout, job.stdoutM)

	// Pipe stderr to collect in jobstatus
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		job.statusM.Lock()
		job.Status.Status = STATUS_COMPLETED
		job.Status.Error = err.Error()
		job.statusM.Unlock()
		return
	}
	go copyPipeToStringArray(stderrPipe, &job.Status.Stderr, job.stderrM)

	////////////////////////////////////////////////////////
	// Start Command                                      //
	////////////////////////////////////////////////////////
	log.Printf("job=%d: Starting the command.", job.Status.JobID)
	cmd.Start()
	log.Printf("job=%d: Command started.", job.Status.JobID)

	// Update status of job after it starts running
	job.statusM.Lock()

	// Set job state to running
	job.Status.Status = STATUS_RUNNING

	// Get PID from command
	job.Status.PID = int64(cmd.Process.Pid)

	job.statusM.Unlock()

	////////////////////////////////////////////////////////////
	// Wait for command to finish                             //
	////////////////////////////////////////////////////////////
	log.Printf("job=%d: Waiting for the command to finish (Pid=%d).", job.Status.JobID, job.Status.PID)
	exitError := cmd.Wait()
	log.Printf("job=%d: Command done.", job.Status.JobID)

	////////////////////////////////////////////////////////////
	// Update job status once command is completed            //
	////////////////////////////////////////////////////////////
	job.statusM.Lock()

	// set finish time of job
	job.Status.FinishTime = time.Now().Unix()
	log.Printf("job=%d: Job finished (status: %d, startTime: %d, finishTime: %d, exitCode: %d, error: %s, "+
		"stdout: \"%s\", stderr: \"%s\".", job.Status.JobID, job.Status.Status, job.Status.StartTime,
		job.Status.FinishTime, job.Status.ExitCode, job.Status.Error, strings.Join(job.Status.Stdout, " "),
		strings.Join(job.Status.Stderr, " "))

	// Collect the exit code of the command
	if exitError != nil {
		if exiterr, ok := exitError.(*exec.ExitError); ok {
			if status, ok := exiterr.Sys().(syscall.WaitStatus); ok {
				job.Status.ExitCode = int64(status.ExitStatus())
				job.Status.Error = exiterr.Error()
			} else {
				job.Status.Error = exiterr.Error()
			}
		} else {
			log.Printf("job=%d: Job finished with error: %v", job.Status.JobID, exitError)
			job.Status.Error = exitError.Error()
		}
	} else {
		// If the command exited with a nil error, then it
		// completed without error, i.e. exit code is 0
		job.Status.ExitCode = 0
	}

	// Set the job state as completed
	job.Status.Status = STATUS_COMPLETED
	job.statusM.Unlock()

	// Nothing to return. Clients will poll the server for job statuses
	return
}

// Get a new unique job id for each new job that gets requested.
// TODO: consider making this a uuid instead of an int
func (s *server) getNewJobID() uint64 {
	s.IDm.Lock()
	defer s.IDm.Unlock()
	myID := s.nextID
	s.nextID++
	return myID
}
