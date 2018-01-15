package channel

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strings"
	"time"

	"strconv"

	"errors"

	"sync"

	"regexp"

	"github.com/aws/amazon-ssm-agent/agent/log"
	"github.com/fsnotify/fsnotify"
)

const (
	defaultFileCreateMode = 0750
	//exclusive flag works on windows, while 660 blocks others access to the file
	defaultFileWriteMode = os.ModeExclusive | 0660
)

//TODO add unittest
type fileWatcherChannel struct {
	logger        log.T
	path          string
	tmpPath       string
	onMessageChan chan string
	mode          Mode
	counter       int
	//the next expected message
	recvCounter int
	startTime   string
	watcher     *fsnotify.Watcher
	mu          sync.RWMutex
	closed      bool
}

//TODO make this constructor private
/*
	Create a file channel, a file channel is identified by its unique name
	name is the path where the watcher directory is created
 	Only Master channel has the privilege to remove the dir at close time
*/
func NewFileWatcherChannel(logger log.T, mode Mode, name string) (*fileWatcherChannel, error) {

	tmpPath := path.Join(name, "tmp")
	curTime := time.Now()
	//TODO if client is RunAs, server needs to grant client user R/W access respectively
	if err := createIfNotExist(name); err != nil {
		logger.Errorf("failed to create directory: %v", err)
		os.RemoveAll(name)
		//if err occurs, the channel is not healthy anymore, should return false
		return nil, err
	}
	if err := createIfNotExist(tmpPath); err != nil {
		logger.Errorf("failed to create directory: %v", err)
		os.RemoveAll(name)
		//if err occurs, the channel is not healthy anymore, should return false
		return nil, err
	}

	//buffered channel in order not to block listener
	onMessageChan := make(chan string, defaultChannelBufferSize)

	//start file watcher and monitor the directory
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		logger.Errorf("filewatcher listener encountered error when start watcher: %v", err)
		os.RemoveAll(name)
		return nil, err
	}

	if err = watcher.Add(name); err != nil {
		logger.Errorf("filewatcher listener encountered error when add watch: %v", err)
		os.RemoveAll(name)
		return nil, err
	}

	ch := &fileWatcherChannel{
		path:          name,
		tmpPath:       tmpPath,
		watcher:       watcher,
		onMessageChan: onMessageChan,
		logger:        logger,
		mode:          mode,
		counter:       0,
		recvCounter:   0,
		startTime:     fmt.Sprintf("%04d%02d%02d%02d%02d%02d", curTime.Year(), curTime.Month(), curTime.Day(), curTime.Hour(), curTime.Minute(), curTime.Second()),
	}
	go ch.watch()
	return ch, nil
}

func createIfNotExist(dir string) (err error) {
	if _, err = os.Stat(dir); os.IsNotExist(err) {
		//configure it to be not accessible by others
		err = os.MkdirAll(dir, defaultFileCreateMode)
	}
	return
}

/*
	drop a file in the destination path with the file name as sequence id
	the file is first named as tmp, then quickly renamed to guarantee atomicity
	sequence id format: {mode}-{command start time}-{counter} , squence id is guaranteed to be ascending order

*/
func (ch *fileWatcherChannel) Send(rawJson string) error {
	if ch.closed {
		return errors.New("channel already closed")
	}
	log := ch.logger
	ch.mu.RLock()
	defer ch.mu.RUnlock()
	sequenceID := fmt.Sprintf("%v-%s-%03d", ch.mode, ch.startTime, ch.counter)
	filepath := path.Join(ch.path, sequenceID)
	tmp_filepath := path.Join(ch.tmpPath, sequenceID)
	//ensure sync exclusive write
	if err := ioutil.WriteFile(tmp_filepath, []byte(rawJson), defaultFileWriteMode); err != nil {
		log.Errorf("write file %v encountered error: %v \n", tmp_filepath, err)
		return err
	}
	if err := os.Rename(tmp_filepath, filepath); err != nil {
		log.Errorf("send renaming file encountered error: %v", err)
		return err
	}
	//file successfully sent, increment counter
	ch.counter++
	return nil
}

func (ch *fileWatcherChannel) GetMessage() <-chan string {
	return ch.onMessageChan
}

func (ch *fileWatcherChannel) Destroy() {
	ch.Close()
	//only master can remove the dir at close
	if ch.mode == ModeMaster {
		ch.logger.Debug("master removing directory...")
		if err := os.RemoveAll(ch.path); err != nil {
			ch.logger.Errorf("failed to remove directory %v : %v", ch.path, err)
		}
	}
}

// Close a filechannel
// non-blocking call, drain the buffered messages and clear file watcher resources
func (ch *fileWatcherChannel) Close() {
	if ch.closed {
		return
	}
	log := ch.logger
	log.Infof("channel %v requested close", ch.path)
	//block other threads to call Send()
	ch.closed = true
	//read all the left over messages
	ch.consumeAll()
	// fsnotify.watch.close() could be a blocking call, we should offload them to a different go-routine
	go func() {
		defer func() {
			if msg := recover(); msg != nil {
				log.Errorf("closing file watcher panics: %v", msg)
			}
			close(ch.onMessageChan)
			log.Infof("channel %v closed", ch.path)
		}()
		//make sure the file watcher closed as well as the watch list is removed, otherwise can cause leak in ubuntu kernel
		ch.watcher.Remove(ch.path)
		ch.watcher.Close()
	}()

	return
}

//parse the counter out of the sequence id, return -1 if parsing fails
//counter is defined as the padding last element of - separated integer
//On windows, path.Base() does not work
func parseSequenceCounter(filepath string) int {
	_, name := path.Split(filepath)
	parts := strings.Split(name, "-")
	counter, err := strconv.ParseInt(parts[len(parts)-1], 10, 64)
	if err != nil {
		return -1
	}
	return int(counter)
}

//read all messages in the consuming dir, with order guarantees -- ioutil.ReadDir() sort by name, and name is the lexicographical ascending sequence id.
//filter out its own sent messages and tmp messages
func (ch *fileWatcherChannel) consumeAll() {
	ch.logger.Debug("consuming all the messages under: ", ch.path)
	fileInfos, _ := ioutil.ReadDir(ch.path)
	if len(fileInfos) > 0 {
		for _, info := range fileInfos {
			name := info.Name()
			if ch.isReadable(name) {
				ch.consume(path.Join(ch.path, name))
			}
		}
	}
}

//TODO add unittest
func (ch *fileWatcherChannel) isReadable(filename string) bool {
	matched, err := regexp.MatchString("[a-zA-Z]+-[0-9]+-[0-9]+", filename)
	if !matched || err != nil {
		return false
	}
	return !strings.Contains(filename, string(ch.mode)) && !strings.Contains(filename, "tmp")
}

//read and remove a given file
func (ch *fileWatcherChannel) consume(filepath string) {
	log := ch.logger
	log.Debugf("consuming message under path: %v", filepath)
	buf, err := ioutil.ReadFile(filepath)
	//On windows rename does not guarantee atomic access: https://github.com/golang/go/issues/8914
	//In exclusive mode we have, this read will for sure fail when it's locked by the other end
	//TODO implement retry
	if err != nil {
		log.Errorf("message %v failed to read: %v \n", filepath, err)
		return

	}

	//remove the consumed file
	os.Remove(filepath)
	//update the recvcounter
	ch.recvCounter = parseSequenceCounter(filepath) + 1
	//TODO handle buffered channel queue overflow
	ch.onMessageChan <- string(buf)
}

// we need to launch watcher receiver in another go routine, putting watcher.Close() and the receiver in same go routine can
// end up dead lock
// make sure this go routine not leaking
func (ch *fileWatcherChannel) watch() {
	log := ch.logger
	log.Debugf("%v listener started on path: %v", ch.mode, ch.path)
	//drain all the current messages in the dir
	ch.consumeAll()
	for {
		select {
		case event, ok := <-ch.watcher.Events:
			if !ok {
				log.Debug("fileWatcher already closed")
				return
			}
			log.Debug("received event: ", event.String())
			if event.Op&fsnotify.Create == fsnotify.Create && ch.isReadable(event.Name) {
				//if the receiving counter is as expected, consume that message
				//otherwise, read the entire directory in sorted order, sender assures sending order
				if parseSequenceCounter(event.Name) == ch.recvCounter {
					ch.consume(event.Name)
				} else {
					log.Debug("received out-of-order file update, polling the dir to reorder")
					ch.consumeAll()
				}
			}
		case err := <-ch.watcher.Errors:
			if err != nil {
				log.Errorf("file watcher error: %v", err)				
			}
		}
	}

}
