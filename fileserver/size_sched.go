package main

import (
	"fmt"
	"log"
	"sync"

	"database/sql"

	"github.com/haiwen/seafile-server/fileserver/commitmgr"
	"github.com/haiwen/seafile-server/fileserver/fsmgr"
	"github.com/haiwen/seafile-server/fileserver/repomgr"
)

type Job struct {
	callback jobCB
	repoID   string
}

type jobCB func(repoID string) error

var jobs = make(chan Job, 10)

// need to start a go routine
func createWorkerPool(n int) {
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go worker(&wg)
	}
	wg.Wait()
}

func worker(wg *sync.WaitGroup) {
	for {
		select {
		case job := <-jobs:
			if job.callback != nil {
				err := job.callback(job.repoID)
				if err != nil {
					log.Printf("failed to call jobs: %v.\n", err)
				}
			}
		default:
		}
	}
	wg.Done()
}

func updateRepoSize(repoID string) {
	job := Job{computeRepoSize, repoID}
	jobs <- job
}

func computeRepoSize(repoID string) error {
	var size int64
	var fileCount int64

	repo := repomgr.Get(repoID)
	if repo == nil {
		err := fmt.Errorf("[scheduler] failed to get repo %s.\n", repoID)
		return err
	}
	info, err := repomgr.GetOldRepoInfo(repoID)
	if err != nil {
		err := fmt.Errorf("[scheduler] failed to get old repo info: %v.\n", err)
		return err
	}

	if info != nil && info.HeadID == repo.HeadCommitID {
		return nil
	}

	head, err := commitmgr.Load(repo.ID, repo.HeadCommitID)
	if err != nil {
		err := fmt.Errorf("[scheduler] failed to get head commit %s.\n", repo.HeadCommitID)
		return err
	}

	var oldHead *commitmgr.Commit
	if info != nil {
		commit, _ := commitmgr.Load(repo.ID, info.HeadID)
		oldHead = commit
	}

	if info != nil && oldHead != nil {
		var results []*diffEntry
		var changeSize int64
		var changeFileCount int64
		err := diffCommits(oldHead, head, &results, false)
		if err != nil {
			err := fmt.Errorf("[scheduler] failed to do diff commits: %v.\n", err)
			return err
		}

		for _, de := range results {
			if de.status == DIFF_STATUS_DELETED {
				changeSize -= de.size
				changeFileCount--
			} else if de.status == DIFF_STATUS_ADDED {
				changeSize += de.size
				changeFileCount++
			} else if de.status == DIFF_STATUS_MODIFIED {
				changeSize = changeSize + de.size + de.originSize
			}
		}
		size = info.Size + changeSize
		fileCount = info.FileCount + changeFileCount
	} else {
		info, err := fsmgr.GetFileCountInfoByPath(repo.StoreID, repo.RootID, "/")
		if err != nil {
			err := fmt.Errorf("[scheduler] failed to get file count.\n")
			return err
		}

		fileCount = info.FileCount
		size = info.Size
	}

	err = setRepoSizeAndFileCount(repoID, repo.HeadCommitID, size, fileCount)
	if err != nil {
		err := fmt.Errorf("[scheduler] failed to set repo size and file count %s: %v.\n", repoID, err)
		return err
	}

	return nil
}

func setRepoSizeAndFileCount(repoID, newHeadID string, size, fileCount int64) error {
	trans, err := seafileDB.Begin()
	if err != nil {
		err := fmt.Errorf("failed to start transaction: %v.\n", err)
		return err
	}

	var headID string
	sqlStr := "SELECT head_id FROM RepoSize WHERE repo_id=?"

	row := trans.QueryRow(sqlStr, repoID)
	if err := row.Scan(&headID); err != nil {
		if err != sql.ErrNoRows {
			trans.Rollback()
			return err
		}
	}

	if headID == "" {
		sqlStr := "INSERT INTO RepoSize (repo_id, size, head_id) VALUES (?, ?, ?)"
		_, err = trans.Exec(sqlStr, repoID, size, newHeadID)
		if err != nil {
			trans.Rollback()
			return err
		}
	} else {
		sqlStr = "UPDATE RepoSize SET size = ?, head_id = ? WHERE repo_id = ?"
		_, err = trans.Exec(sqlStr, size, newHeadID, repoID)
		if err != nil {
			trans.Rollback()
			return err
		}
	}

	var exist int
	sqlStr = "SELECT 1 FROM RepoFileCount WHERE repo_id=?"
	row = trans.QueryRow(sqlStr, repoID)
	if err := row.Scan(&exist); err != nil {
		if err != sql.ErrNoRows {
			trans.Rollback()
			return err
		}
	}

	if exist != 0 {
		sqlStr := "UPDATE RepoFileCount SET file_count=? WHERE repo_id=?"
		_, err = trans.Exec(sqlStr, fileCount, repoID)
		if err != nil {
			trans.Rollback()
			return err
		}
	} else {
		sqlStr := "INSERT INTO RepoFileCount (repo_id,file_count) VALUES (?,?)"
		_, err = trans.Exec(sqlStr, repoID, fileCount)
		if err != nil {
			trans.Rollback()
			return err
		}
	}

	trans.Commit()

	return nil
}
