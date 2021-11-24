/*
Copyright 2021 Loggie Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package file

import (
	"crypto/md5"
	"fmt"
	"io"
	"loggie.io/loggie/pkg/core/log"
	"loggie.io/loggie/pkg/util"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	JobActive          = JobStatus(1)
	JobDelete          = JobStatus(2)
	JobStop            = JobStatus(3)
	JobStopImmediately = JobStatus(999)

	defaultIdentifier = "BLANK"
)

var globalJobIndex uint32

type JobStatus int

type Job struct {
	uid               string
	watchUid          string
	watchUidLen       int
	index             uint32
	filename          string
	aFileName         atomic.Value
	file              *os.File
	status            JobStatus
	aStatus           atomic.Value
	endOffset         int64
	nextOffset        int64
	currentLineNumber int64
	currentLines      int64
	eofCount          int
	lastActiveTime    time.Time
	deleteTime        time.Time
	renameTime        time.Time
	identifier        string

	task *WatchTask
}

func JobUid(fileInfo os.FileInfo) string {
	stat := fileInfo.Sys().(*syscall.Stat_t)
	inode := stat.Ino
	device := uint64(stat.Dev)
	var buf [64]byte
	current := strconv.AppendUint(buf[:0], inode, 10)
	current = append(current, '-')
	current = strconv.AppendUint(current, device, 10)
	return string(current)
}

func WatchJobId(pipelineName string, sourceName string, jobUid string) string {
	var watchJobId strings.Builder
	watchJobId.WriteString(pipelineName)
	watchJobId.WriteString(":")
	watchJobId.WriteString(sourceName)
	watchJobId.WriteString(":")
	watchJobId.WriteString(jobUid)
	return watchJobId.String()
}

// WatchUid Support repeated collection of the same file by different sources
func (j *Job) WatchUid() string {
	if j.watchUid != "" {
		return j.watchUid
	}
	wj := WatchJobId(j.task.pipelineName, j.task.sourceName, j.Uid())
	j.watchUid = wj
	j.watchUidLen = len(j.watchUid)
	return wj
}

func (j *Job) Uid() string {
	return j.uid
}
func (j *Job) Index() uint32 {
	return j.index
}

func (j *Job) Delete() {
	j.ChangeStatusTo(JobDelete)
	j.deleteTime = time.Now()
}

func (j *Job) IsDelete() bool {
	return j.status == JobDelete || !j.deleteTime.IsZero()
}

func (j *Job) Stop() {
	j.ChangeStatusTo(JobStop)
}

func (j *Job) ChangeStatusTo(status JobStatus) {
	j.status = status
	j.aStatus.Store(status)
}

func (j *Job) Release() {
	if j.file == nil {
		return
	}
	err := j.file.Close()
	if err != nil {
		log.Error("release job(fileName: %s) error: %s", j.filename, err)
	}
	j.file = nil
	log.Info("job(fileName: %s) has been released", j.filename)
}

func (j *Job) Sync() {
	j.status = j.aStatus.Load().(JobStatus)
	j.filename = j.aFileName.Load().(string)
}

func (j *Job) RenameTo(newFilename string) {
	j.filename = newFilename
	j.aFileName.Store(newFilename)
	j.renameTime = time.Now()
}

func (j *Job) IsRename() bool {
	return !j.renameTime.IsZero()
}

func (j *Job) Active() error {
	if j.file == nil {
		// reopen
		file, err := os.Open(j.filename)
		if err != nil {
			if os.IsPermission(err) {
				log.Error("no permission for filename: %s", j.filename)
			}
			return err
		}
		j.file = file

		fileInfo, err := file.Stat()
		if err != nil {
			return err
		}
		newUid := JobUid(fileInfo)
		if j.Uid() != newUid {
			return fmt.Errorf("job(filename: %s) uid(%s) changed to %s，it maybe not a file", j.filename, j.Uid(), newUid)
		}

		// reset file offset and lineNumber
		if j.nextOffset != 0 {
			_, err = file.Seek(j.nextOffset, io.SeekStart)
			if err != nil {
				return err
			}
			// init lineNumber
			if j.currentLineNumber == 0 {
				lineNumber, err := util.LineCountTo(j.nextOffset, j.filename)
				if err != nil {
					return err
				}
				j.currentLineNumber = int64(lineNumber)
			}
		}
	}
	j.status = JobActive
	j.aStatus.Store(JobActive)
	j.eofCount = 0
	j.lastActiveTime = time.Now()
	return nil
}

func (j *Job) NextOffset(offset int64) {
	if offset > 0 {
		j.nextOffset = offset
	}
}

func (j *Job) GenerateIdentifier() error {
	if j.identifier != "" {
		return nil
	}
	stat, err := os.Stat(j.filename)
	if err != nil {
		return err
	}
	readSize := j.task.config.FirstNBytesForIdentifier
	fileSize := stat.Size()
	if fileSize < int64(readSize) {
		return fmt.Errorf("file size is smaller than firstNBytesForIdentifier: %d < %d", fileSize, readSize)
	}
	file, err := os.Open(j.filename)
	defer file.Close()
	if err != nil {
		return err
	}
	readBuffer := make([]byte, readSize)
	l, err := file.Read(readBuffer)
	if err != nil {
		return err
	}
	if l < readSize {
		return fmt.Errorf("read size is smaller than firstNBytesForIdentifier: %d < %d", l, readSize)
	}
	j.identifier = fmt.Sprintf("%x", md5.Sum(readBuffer))
	return nil
}

func (j *Job) IsSame(other *Job) bool {
	if other == nil {
		return false
	}
	if j == other {
		return true
	}
	if j.WatchUid() != other.WatchUid() {
		return false
	}
	return j.identifier == other.identifier
}

func (j *Job) Read() {
	j.task.activeChan <- j
}

func (j *Job) ProductEvent(endOffset int64, collectTime time.Time, body []byte) {
	nextOffset := endOffset + 1
	contentBytes := int64(len(body))
	// -1 because `\n`
	startOffset := nextOffset - contentBytes - 1

	j.currentLineNumber++
	j.currentLines++
	j.endOffset = endOffset
	j.nextOffset = nextOffset
	watchUid := j.WatchUid()

	endOffsetStr := strconv.FormatInt(endOffset, 10)
	var eventUid strings.Builder
	eventUid.Grow(j.watchUidLen + 1 + len(endOffsetStr))
	eventUid.WriteString(watchUid)
	eventUid.WriteString("-")
	eventUid.WriteString(endOffsetStr)
	state := &State{
		Epoch:        j.task.epoch,
		PipelineName: j.task.pipelineName,
		SourceName:   j.task.sourceName,
		Offset:       startOffset,
		NextOffset:   nextOffset,
		LineNumber:   j.currentLineNumber,
		Filename:     j.filename,
		CollectTime:  collectTime,
		ContentBytes: contentBytes + 1, // because `\n`
		JobUid:       j.Uid(),
		JobIndex:     j.Index(),
		watchUid:     watchUid,
		EventUid:     eventUid.String(),
	}
	header := map[string]interface{}{
		SystemStateKey: state,
	}
	e := j.task.eventPool.Get()
	// copy body,because readBuffer reuse
	contentBuffer := make([]byte, contentBytes)
	copy(contentBuffer, body)
	e.Fill(header, contentBuffer)
	j.task.productFunc(e)
}

func NewJob(task *WatchTask, filename string, fileInfo os.FileInfo) *Job {
	jobUid := JobUid(fileInfo)
	return newJobWithUid(task, filename, jobUid)
}

func newJobWithUid(task *WatchTask, filename string, jobUid string) *Job {
	j := &Job{
		task:     task,
		index:    jobIndex(),
		filename: filename,
		uid:      jobUid,
	}
	j.aFileName.Store(filename)
	return j
}

func jobIndex() uint32 {
	return atomic.AddUint32(&globalJobIndex, 1)
}