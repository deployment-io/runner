package commands

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// go-git's own "file" transport runs the host's git-upload-pack and
// git-receive-pack. For a clone or fetch between two directories on this host
// — the start-up bump's clone of a Step's checkout and its fetch back, and a
// dependency bump's clone of its library mirror — that means host git reading
// the source repository's config, and refusing ("detected dubious ownership")
// when the repository is owned by another user. A Step's checkout is: it is
// handed to the agentbox user after the checkout (chownTaskCheckout), and on a
// re-run its .git was written by the agent, whose config can name commands for
// git to run. So local transfers are served in-process by go-git's own server
// instead: nothing runs on the host and no git config is acted on.
func init() {
	client.InstallProtocol("file", server.NewClient(localRepositoryLoader{}))
}

// localRepositoryLoader opens the repository at an endpoint's local path for
// go-git's in-process server. go-git's server.DefaultLoader can't be used: it
// accepts a work tree with a .git directory but then opens the storage at the
// work tree itself, where no objects are.
type localRepositoryLoader struct{}

func (localRepositoryLoader) Load(ep *transport.Endpoint) (storer.Storer, error) {
	gitDir, err := localGitDir(ep.Path)
	if err != nil {
		return nil, err
	}
	return filesystem.NewStorage(osfs.New(gitDir), cache.NewObjectLRUDefault()), nil
}

// localGitDir is the git directory of the repository at path: path/.git for a
// work tree, or path itself for a bare repository (a regular "config" file and
// no .git). A .git that isn't a real directory — a symlink, which could point
// anywhere on the host, or a "gitdir:" file — is refused, as is a path holding
// neither: transport.ErrRepositoryNotFound.
func localGitDir(path string) (string, error) {
	dotGit := filepath.Join(path, git.GitDirName)
	if info, err := os.Lstat(dotGit); err == nil {
		if !info.IsDir() {
			return "", transport.ErrRepositoryNotFound
		}
		return dotGit, nil
	}
	if info, err := os.Lstat(filepath.Join(path, "config")); err == nil && info.Mode().IsRegular() {
		return path, nil
	}
	return "", transport.ErrRepositoryNotFound
}

// copyBumpCommit copies the commit hash, one commit on parent, from one
// repository's storage into another's that already has parent: the commit, and
// the trees and blobs of its tree that differ from parent's tree. Submodule
// entries (gitlinks) name commits of other repositories and aren't copied.
// In-process: no transport and no git command.
func copyBumpCommit(from, to storer.EncodedObjectStorer, hash, parent plumbing.Hash) error {
	commit, err := object.GetCommit(from, hash)
	if err != nil {
		return err
	}
	if len(commit.ParentHashes) != 1 || commit.ParentHashes[0] != parent {
		return errors.New("the bump commit isn't one commit on the checkout")
	}
	parentCommit, err := object.GetCommit(to, parent)
	if err != nil {
		return err
	}
	tree, err := commit.Tree()
	if err != nil {
		return err
	}
	parentTree, err := parentCommit.Tree()
	if err != nil {
		return err
	}
	hashes := []plumbing.Hash{hash}
	if err := newTreeObjects(tree, parentTree, &hashes); err != nil {
		return err
	}
	for _, h := range hashes {
		obj, err := from.EncodedObject(plumbing.AnyObject, h)
		if err != nil {
			return err
		}
		if _, err := to.SetEncodedObject(obj); err != nil {
			return err
		}
	}
	return nil
}

// newTreeObjects appends to out tree's hash and those of the trees and blobs
// under it that old (nil: none) doesn't have at the same path with the same
// hash.
func newTreeObjects(tree, old *object.Tree, out *[]plumbing.Hash) error {
	if old != nil && old.Hash == tree.Hash {
		return nil
	}
	*out = append(*out, tree.Hash)
	for _, entry := range tree.Entries {
		var oldEntry *object.TreeEntry
		if old != nil {
			for i := range old.Entries {
				if old.Entries[i].Name == entry.Name {
					oldEntry = &old.Entries[i]
					break
				}
			}
		}
		if oldEntry != nil && oldEntry.Hash == entry.Hash {
			continue
		}
		switch entry.Mode {
		case filemode.Submodule:
			continue
		case filemode.Dir:
			sub, err := tree.Tree(entry.Name)
			if err != nil {
				return err
			}
			var oldSub *object.Tree
			if oldEntry != nil && oldEntry.Mode == filemode.Dir {
				if oldSub, err = old.Tree(entry.Name); err != nil {
					return err
				}
			}
			if err := newTreeObjects(sub, oldSub, out); err != nil {
				return err
			}
		default:
			*out = append(*out, entry.Hash)
		}
	}
	return nil
}
