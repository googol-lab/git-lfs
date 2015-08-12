package commands

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"strings"

	"github.com/github/git-lfs/lfs"
	"github.com/github/git-lfs/vendor/_nuts/github.com/spf13/cobra"
)

var (
	prePushCmd = &cobra.Command{
		Use:   "pre-push",
		Short: "Implements the Git pre-push hook",
		Run:   prePushCommand,
	}
	prePushDryRun        = false
	prePushDeleteBranch  = "(delete)"
	prePushMissingErrMsg = "%s is an LFS pointer to %s, which does not exist in .git/lfs/objects.\n\nRun 'git lfs fsck' to verify Git LFS objects."
)

// prePushCommand is run through Git's pre-push hook. The pre-push hook passes
// two arguments on the command line:
//
//   1. Name of the remote to which the push is being done
//   2. URL to which the push is being done
//
// The hook receives commit information on stdin in the form:
//   <local ref> <local sha1> <remote ref> <remote sha1>
//
// In the typical case, prePushCommand will get a list of git objects being
// pushed by using the following:
//
//    git rev-list --objects <local sha1> ^<remote sha1>
//
// If any of those git objects are associated with Git LFS objects, those
// objects will be pushed to the Git LFS API.
//
// In the case of pushing a new branch, the list of git objects will be all of
// the git objects in this branch.
//
// In the case of deleting a branch, no attempts to push Git LFS objects will be
// made.
func prePushCommand(cmd *cobra.Command, args []string) {
	var left, right string

	if len(args) == 0 {
		Print("This should be run through Git's pre-push hook.  Run `git lfs update` to install it.")
		os.Exit(1)
	}

	lfs.Config.CurrentRemote = args[0]

	refsData, err := ioutil.ReadAll(os.Stdin)
	if err != nil {
		Panic(err, "Error reading refs on stdin")
	}

	if len(refsData) == 0 {
		return
	}

	left, right = decodeRefs(string(refsData))
	if left == prePushDeleteBranch {
		return
	}

	// Just use scanner here
	pointers, err := lfs.ScanRefs(left, right, nil)
	if err != nil {
		Panic(err, "Error scanning for Git LFS files")
	}

	totalSize := int64(0)
	for _, p := range pointers {
		totalSize += p.Size
	}

	// Objects to skip because they're missing locally but on server
	var skipObjects map[string]struct{}

	if !prePushDryRun {
		// Do this as a pre-flight check since upload queue starts immediately
		var err error
		skipObjects, err = prePushCheckForMissingObjects(pointers)
		if err != nil {
			Panic(errors.New("Missing objects to push"), err.Error())
		}
	}

	uploadQueue := lfs.NewUploadQueue(len(pointers), totalSize, prePushDryRun)

	for _, pointer := range pointers {
		if prePushDryRun {
			Print("push %s", pointer.Name)
			continue
		}

		if _, skip := skipObjects[pointer.Oid]; skip {
			// object missing locally but on server, don't bother
			continue
		}

		u, wErr := lfs.NewUploadable(pointer.Oid, pointer.Name)
		if wErr != nil {
			if cleanPointerErr, ok := wErr.Err.(*lfs.CleanedPointerError); ok {
				Exit(prePushMissingErrMsg, pointer.Name, cleanPointerErr.Pointer.Oid)
			} else if Debugging || wErr.Panic {
				Panic(wErr.Err, wErr.Error())
			} else {
				Exit(wErr.Error())
			}
		}

		uploadQueue.Add(u)
	}

	if !prePushDryRun {
		uploadQueue.Wait()
		for _, err := range uploadQueue.Errors() {
			if Debugging || err.Panic {
				LoggedError(err.Err, err.Error())
			} else {
				Error(err.Error())
			}
		}

		if len(uploadQueue.Errors()) > 0 {
			os.Exit(2)
		}
	}
}

func prePushCheckForMissingObjects(pointers []*lfs.WrappedPointer) (objectsOnServer map[string]struct{}, e error) {
	var missingLocalObjects []*lfs.WrappedPointer
	var missingSize int64
	var skipObjects = make(map[string]struct{}, len(pointers))
	for _, pointer := range pointers {
		if !lfs.ObjectExistsOfSize(pointer.Oid, pointer.Size) {
			// We think we need to push this but we don't have it
			// Store for server checking later
			missingLocalObjects = append(missingLocalObjects, pointer)
			missingSize += pointer.Size
			skipObjects[pointer.Oid] = struct{}{}
		}
	}
	if len(missingLocalObjects) == 0 {
		return nil, nil
	}

	checkQueue := lfs.NewCheckQueue(len(missingLocalObjects), missingSize, false)
	for _, p := range missingLocalObjects {
		checkQueue.Add(lfs.NewCheckable(p))
	}
	checkQueue.Wait()
	if len(checkQueue.Errors()) > 0 {
		// Extract oids from messages & collate
		var combinedMsg bytes.Buffer
		for _, wrerr := range checkQueue.Errors() {
			oid := wrerr.Get("oid")
			name := wrerr.Get("name")
			combinedMsg.WriteString(fmt.Sprintf(prePushMissingErrMsg, name, oid))
			combinedMsg.WriteString("\n")
		}
		return nil, errors.New(combinedMsg.String())
	}
	return skipObjects, nil
}

// decodeRefs pulls the sha1s out of the line read from the pre-push
// hook's stdin.
func decodeRefs(input string) (string, string) {
	refs := strings.Split(strings.TrimSpace(input), " ")
	var left, right string

	if len(refs) > 1 {
		left = refs[1]
	}

	if len(refs) > 3 {
		right = "^" + refs[3]
	}

	return left, right
}

func init() {
	prePushCmd.Flags().BoolVarP(&prePushDryRun, "dry-run", "d", false, "Do everything except actually send the updates")
	RootCmd.AddCommand(prePushCmd)
}
