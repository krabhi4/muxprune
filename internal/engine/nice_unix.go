//go:build unix

package engine

import "os/exec"

// wrapNice prepends nice/ionice to the command so remux subprocesses run at
// the lowest scheduling priority, keeping the host responsive.
func wrapNice(name string, args []string) (string, []string) {
	nicePath, err := exec.LookPath("nice")
	if err != nil {
		return name, args
	}
	wrapped := []string{"-n", "19"}
	if ionicePath, err := exec.LookPath("ionice"); err == nil {
		wrapped = append(wrapped, ionicePath, "-c", "3")
	}
	wrapped = append(wrapped, name)
	wrapped = append(wrapped, args...)
	return nicePath, wrapped
}
