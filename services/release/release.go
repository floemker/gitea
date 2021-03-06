// Copyright 2019 The Gitea Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package release

import (
	"fmt"
	"strings"

	"code.gitea.io/gitea/models"
	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/notification"
	"code.gitea.io/gitea/modules/repository"
	"code.gitea.io/gitea/modules/storage"
	"code.gitea.io/gitea/modules/timeutil"
)

func createTag(gitRepo *git.Repository, rel *models.Release) error {
	// Only actual create when publish.
	if !rel.IsDraft {
		if !gitRepo.IsTagExist(rel.TagName) {
			commit, err := gitRepo.GetCommit(rel.Target)
			if err != nil {
				return fmt.Errorf("GetCommit: %v", err)
			}

			// Trim '--' prefix to prevent command line argument vulnerability.
			rel.TagName = strings.TrimPrefix(rel.TagName, "--")
			if err = gitRepo.CreateTag(rel.TagName, commit.ID.String()); err != nil {
				if strings.Contains(err.Error(), "is not a valid tag name") {
					return models.ErrInvalidTagName{
						TagName: rel.TagName,
					}
				}
				return err
			}
			rel.LowerTagName = strings.ToLower(rel.TagName)
			// Prepare Notify
			if err := rel.LoadAttributes(); err != nil {
				log.Error("LoadAttributes: %v", err)
				return err
			}
			notification.NotifyPushCommits(
				rel.Publisher, rel.Repo,
				&repository.PushUpdateOptions{
					RefFullName: git.TagPrefix + rel.TagName,
					OldCommitID: git.EmptySHA,
					NewCommitID: commit.ID.String(),
				}, repository.NewPushCommits())
			notification.NotifyCreateRef(rel.Publisher, rel.Repo, "tag", git.TagPrefix+rel.TagName)
			rel.CreatedUnix = timeutil.TimeStampNow()
		}
		commit, err := gitRepo.GetTagCommit(rel.TagName)
		if err != nil {
			return fmt.Errorf("GetTagCommit: %v", err)
		}

		rel.Sha1 = commit.ID.String()
		rel.NumCommits, err = commit.CommitsCount()
		if err != nil {
			return fmt.Errorf("CommitsCount: %v", err)
		}

		u, err := models.GetUserByEmail(commit.Author.Email)
		if err == nil {
			rel.PublisherID = u.ID
		}

	} else {
		rel.CreatedUnix = timeutil.TimeStampNow()
	}
	return nil
}

// CreateRelease creates a new release of repository.
func CreateRelease(gitRepo *git.Repository, rel *models.Release, attachmentUUIDs []string) error {
	isExist, err := models.IsReleaseExist(rel.RepoID, rel.TagName)
	if err != nil {
		return err
	} else if isExist {
		return models.ErrReleaseAlreadyExist{
			TagName: rel.TagName,
		}
	}

	if err = createTag(gitRepo, rel); err != nil {
		return err
	}

	rel.LowerTagName = strings.ToLower(rel.TagName)
	if err = models.InsertRelease(rel); err != nil {
		return err
	}

	if err = models.AddReleaseAttachments(rel.ID, attachmentUUIDs); err != nil {
		return err
	}

	if !rel.IsDraft {
		notification.NotifyNewRelease(rel)
	}

	return nil
}

// UpdateReleaseOrCreatReleaseFromTag updates information of a release or create release from tag.
func UpdateReleaseOrCreatReleaseFromTag(doer *models.User, gitRepo *git.Repository, rel *models.Release, attachmentUUIDs []string, isCreate bool) (err error) {
	if err = createTag(gitRepo, rel); err != nil {
		return err
	}
	rel.LowerTagName = strings.ToLower(rel.TagName)

	if err = models.UpdateRelease(models.DefaultDBContext(), rel); err != nil {
		return err
	}

	if err = models.AddReleaseAttachments(rel.ID, attachmentUUIDs); err != nil {
		log.Error("AddReleaseAttachments: %v", err)
	}

	if !isCreate {
		notification.NotifyUpdateRelease(doer, rel)
		return
	}

	if !rel.IsDraft {
		notification.NotifyNewRelease(rel)
	}

	return err
}

// DeleteReleaseByID deletes a release and corresponding Git tag by given ID.
func DeleteReleaseByID(id int64, doer *models.User, delTag bool) error {
	rel, err := models.GetReleaseByID(id)
	if err != nil {
		return fmt.Errorf("GetReleaseByID: %v", err)
	}

	repo, err := models.GetRepositoryByID(rel.RepoID)
	if err != nil {
		return fmt.Errorf("GetRepositoryByID: %v", err)
	}

	if delTag {
		if stdout, err := git.NewCommand("tag", "-d", rel.TagName).
			SetDescription(fmt.Sprintf("DeleteReleaseByID (git tag -d): %d", rel.ID)).
			RunInDir(repo.RepoPath()); err != nil && !strings.Contains(err.Error(), "not found") {
			log.Error("DeleteReleaseByID (git tag -d): %d in %v Failed:\nStdout: %s\nError: %v", rel.ID, repo, stdout, err)
			return fmt.Errorf("git tag -d: %v", err)
		}

		if err := models.DeleteReleaseByID(id); err != nil {
			return fmt.Errorf("DeleteReleaseByID: %v", err)
		}
	} else {
		rel.IsTag = true

		if err = models.UpdateRelease(models.DefaultDBContext(), rel); err != nil {
			return fmt.Errorf("Update: %v", err)
		}
	}

	rel.Repo = repo
	if err = rel.LoadAttributes(); err != nil {
		return fmt.Errorf("LoadAttributes: %v", err)
	}

	if err := models.DeleteAttachmentsByRelease(rel.ID); err != nil {
		return fmt.Errorf("DeleteAttachments: %v", err)
	}

	for i := range rel.Attachments {
		attachment := rel.Attachments[i]
		if err := storage.Attachments.Delete(attachment.RelativePath()); err != nil {
			log.Error("Delete attachment %s of release %s failed: %v", attachment.UUID, rel.ID, err)
		}
	}

	notification.NotifyDeleteRelease(doer, rel)

	return nil
}
