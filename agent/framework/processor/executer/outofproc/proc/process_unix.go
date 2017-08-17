// Copyright 2016 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may not
// use this file except in compliance with the License. A copy of the
// License is located at
//
// http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing
// permissions and limitations under the License.

// +build darwin freebsd linux netbsd openbsd

// Package process wraps up the os.Process interface and also provides os-specific process lookup functions
package proc

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

//Unix man: http://www.skrenta.com/rt/man/ps.1.html , return the process table of the current user, in agent it'll be root
//verified on RHEL, Amazon Linux, Ubuntu, Centos, FreeBSD and Darwin
var ps = func() ([]byte, error) {
	return exec.Command("ps", "-o", "pid,start").CombinedOutput()
}

//given the pid and the unix process startTime format string, return whether the process is still alive
func find_process(pid int, startTime string) (bool, error) {
	output, err := ps()
	if err != nil {
		return false, err
	}
	proc_list := strings.Split(string(output), "\n")

	for i := 1; i < len(proc_list); i++ {
		parts := strings.Fields(proc_list[i])
		if len(parts) != 2 {
			continue
		}
		_pid, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return false, err
		}
		if pid == int(_pid) && startTime == parts[1] {
			return true, nil
		}
	}
	return false, nil
}

//return the current time in format hour:min:sec which is compliant to unix ps output format
//TODO darwin used a differnt format, need to compile a separate file once we start to support darwin
func get_current_time() string {
	curtime := time.Now()
	return fmt.Sprintf("%02d:%02d:%02d", curtime.Hour(), curtime.Minute(), curtime.Second())
}