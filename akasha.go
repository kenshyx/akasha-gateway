// Akasha Gateway - API for legacy (web 2.0) applications
// Copyright (c) 2018 Péter Szilágyi. All rights reserved.
//
// The Akasha gateway is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or (at your
// option) any later version.
//
// The Akasha gateway is distributed in the hope that it will be useful, but
// WITHOUT ANY WARRANTY; without even the implied warranty of MERCHANTABILITY
// or FITNESS FOR A PARTICULAR PURPOSE. See the GNU Lesser General Public
// License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the Akasha gateway. If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcutil/base58"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/karalabe/akasha-gateway/contracts"
)

var (
	// errUnknownUser is returned if a user cannot be found in the Akasha network.
	errUnknownUser = errors.New("unknown user")

	// errUnknownEntry is returned if an entry cannot be found in the Akasha network.
	errUnknownEntry = errors.New("unknown entry")

	// errUnknownComment is returned if a comment cannot be found in the Akasha network.
	errUnknownComment = errors.New("unknown comment")
)

// akasha represents the interface to the Akasha smart contracts.
type akasha struct {
	eth  *ethclient.Client
	ipfs *ipfs

	aeth      *contracts.AETH
	essence   *contracts.Essence
	resolver  *contracts.ProfileResolver
	registrar *contracts.ProfileRegistrar
	entries   *contracts.Entries
	comments  *contracts.Comments
	feed      *contracts.Feed
}

// config represents the configurations for the Akasha smart contract system.
type config struct {
	AETHAddress      common.Address
	EssenceAddress   common.Address
	ResolverAddress  common.Address
	RegistrarAddress common.Address
	EntriesAddress   common.Address
	CommentsAddress  common.Address
}

// makeAkasha creates a programatic interface to the Akasha smart contracts.
func makeAkasha(geth *node.Node, ipfs *ipfs, conf *config) (*akasha, error) {
	// Attach to the Geth client and bind the Akasha contracts
	rpc, err := geth.Attach()
	if err != nil {
		log.Crit("Failed to attach to Ethereum client", "err", err)
	}
	client := ethclient.NewClient(rpc)

	aeth, err := contracts.NewAETH(conf.AETHAddress, client)
	if err != nil {
		return nil, err
	}
	essence, err := contracts.NewEssence(conf.EssenceAddress, client)
	if err != nil {
		return nil, err
	}
	resolver, err := contracts.NewProfileResolver(conf.ResolverAddress, client)
	if err != nil {
		return nil, err
	}
	registrar, err := contracts.NewProfileRegistrar(conf.RegistrarAddress, client)
	if err != nil {
		return nil, err
	}
	entries, err := contracts.NewEntries(conf.EntriesAddress, client)
	if err != nil {
		return nil, err
	}
	comments, err := contracts.NewComments(conf.CommentsAddress, client)
	if err != nil {
		return nil, err
	}
	return &akasha{
		eth:       client,
		ipfs:      ipfs,
		aeth:      aeth,
		essence:   essence,
		resolver:  resolver,
		registrar: registrar,
		entries:   entries,
		comments:  comments,
	}, nil
}

// User represents all the known information about an Akasha user. The reason
// beind the nullable strings is to allow signalling unreachable IPFS content.
type User struct {
	User     string         `json:"user"`
	Name     *string        `json:"name"`
	Address  common.Address `json:"address"`
	About    *string        `json:"about"`
	Avatar   *string        `json:"avatar"`
	Cover    *string        `json:"cover"`
	Links    []string       `json:"links"`
	Tips     bool           `json:"tips"`
	Aether   *hexutil.Big   `json:"aether"`
	Bonded   *hexutil.Big   `json:"bonded"`
	Cycling  *hexutil.Big   `json:"cycling"`
	Mana     *hexutil.Big   `json:"mana"`
	Spent    *hexutil.Big   `json:"spent"`
	Essence  *hexutil.Big   `json:"essence"`
	Karma    *hexutil.Big   `json:"karma"`
	Entries  uint64         `json:"entries"`
	Comments uint64         `json:"comments"`
}

// image represents a serialize format of an Akasha image with multiple possbile
// resolutions.
type image struct {
	ExtraSmall struct {
		Src string `json:"src"`
	} `json:"xs"`
	Small struct {
		Src string `json:"src"`
	} `json:"sm"`
	Medium struct {
		Src string `json:"src"`
	} `json:"md"`
	Large struct {
		Src string `json:"src"`
	} `json:"xl"`
	ExtraLarge struct {
		Src string `json:"src"`
	} `json:"xxl"`
}

// source returns the image source with the highest resolution.
func (img *image) source() string {
	if img.ExtraLarge.Src != "" {
		return img.ExtraLarge.Src
	}
	if img.Large.Src != "" {
		return img.Large.Src
	}
	if img.Medium.Src != "" {
		return img.Medium.Src
	}
	if img.Small.Src != "" {
		return img.Small.Src
	}
	if img.ExtraSmall.Src != "" {
		return img.ExtraSmall.Src
	}
	return ""
}

// UserByAddress does a reverse ENS lookup to get the registration node of the
// user and retrieves all known infos associated with it.
func (a *akasha) UserByAddress(addr common.Address, timeout time.Duration) (*User, error) {
	node, err := a.resolver.Reverse(nil, addr)
	if err != nil {
		return nil, err
	}
	return a.user(node, timeout)
}

// UserByName does an ENS lookup to get the registration node of the user and
// retrieves all known infos associated with it.
func (a *akasha) UserByName(name string, timeout time.Duration) (*User, error) {
	var label [32]byte
	copy(label[:], name)

	node, err := a.registrar.Hash(nil, label)
	if err != nil {
		return nil, err
	}
	return a.user(node, timeout)
}

// user retrieves all the known infos about a user identified by it's ENS node.
func (a *akasha) user(node [32]byte, timeout time.Duration) (*User, error) {
	// Retrieve the profile infos from the profile resolver
	profile, err := a.resolver.Resolve(nil, node)
	if err != nil {
		return nil, err
	}
	if profile.Addr == (common.Address{}) {
		return nil, errUnknownUser
	}
	// Retrieve profile details from the IPFS profile objects (failures are fine-ish)
	root := base58.Encode(append([]byte{profile.Fn, profile.DigestSize}, profile.Hash[:]...))

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var (
		name   *string
		about  *string
		avatar *string
		cover  *string
		links  []string
	)
	objs, err := a.ipfs.Links(ctx, root)
	if err == nil {
		avatar = new(string)
		*avatar = "https://ipfs.io/ipfs/" + objs["avatar"]

		var (
			blob []byte
			bg   image
			prof struct {
				FirstName string `json:"firstName"`
				LastName  string `json:"lastName"`
			}
			urls []struct {
				Url string `json:"url"`
			}
		)
		if blob, err = a.ipfs.Content(ctx, root); err == nil {
			if err = json.Unmarshal(blob, &prof); err != nil {
				return nil, err
			}
			name = new(string)
			*name = prof.FirstName + " " + prof.LastName
		}
		if blob, err = a.ipfs.Content(ctx, objs["about"]); err == nil {
			if err = json.Unmarshal(blob, &about); err != nil {
				return nil, err
			}
		}
		if blob, err = a.ipfs.Content(ctx, objs["backgroundImage"]); err == nil {
			if err = json.Unmarshal(blob, &bg); err != nil {
				return nil, err
			}
			if src := bg.source(); src != "" {
				cover = new(string)
				*cover = "https://ipfs.io/ipfs/" + src
			}
		}
		if blob, err = a.ipfs.Content(ctx, objs["links"]); err == nil {
			if err = json.Unmarshal(blob, &urls); err != nil {
				return nil, err
			}
			for _, url := range urls {
				links = append(links, url.Url)
			}
		}
	}
	// Retrieve the token infos from the ledger contract
	balances, err := a.aeth.GetTokenRecords(nil, profile.Addr)
	if err != nil {
		return nil, err
	}
	credits, err := a.essence.GetCollected(nil, profile.Addr)
	if err != nil {
		return nil, err
	}
	mana, err := a.essence.Mana(nil, profile.Addr)
	if err != nil {
		return nil, err
	}
	// Retrieve user post statistics
	entries, err := a.entries.GetEntryCount(nil, profile.Addr)
	if err != nil {
		return nil, err
	}
	comments, err := a.comments.TotalCommentsOf(nil, profile.Addr)
	if err != nil {
		return nil, err
	}
	return &User{
		User:     strings.TrimRight(string(profile.AkashaId[:]), string([]byte{0})),
		Name:     name,
		Address:  profile.Addr,
		About:    about,
		Avatar:   avatar,
		Cover:    cover,
		Links:    links,
		Tips:     profile.DonationsEnabled,
		Aether:   (*hexutil.Big)(balances.Free),
		Bonded:   (*hexutil.Big)(balances.Bonded),
		Cycling:  (*hexutil.Big)(balances.Cycling),
		Mana:     (*hexutil.Big)(mana.Total),
		Spent:    (*hexutil.Big)(mana.Spent),
		Essence:  (*hexutil.Big)(credits.Essence),
		Karma:    (*hexutil.Big)(credits.Karma),
		Entries:  entries.Uint64(),
		Comments: comments.Uint64(),
	}, nil
}

// EntriesByAddress retrieves a list of entires posted by a user given its Ethereum
// address.
func (a *akasha) EntriesByAddress(addr common.Address, timeout time.Duration) ([]common.Hash, error) {
	// Filter the Ethereum events for Akasha entry publishes
	it, err := a.entries.FilterPublish(nil, []common.Address{addr}, nil)
	if err != nil {
		return nil, err
	}
	defer it.Close()

	// Gather all the entry ids and return them to the user
	var ids []common.Hash
	for it.Next() {
		ids = append(ids, it.Event.EntryId)
	}
	return ids, nil
}

// EntriesByName retrieves a list of entires posted by a user given its Akasha
// username.
func (a *akasha) EntriesByName(name string, timeout time.Duration) ([]common.Hash, error) {
	// Resolve the user's address from its ID
	var label [32]byte
	copy(label[:], name)

	node, err := a.registrar.Hash(nil, label)
	if err != nil {
		return nil, err
	}
	profile, err := a.resolver.Resolve(nil, node)
	if err != nil {
		return nil, err
	}
	if profile.Addr == (common.Address{}) {
		return nil, errUnknownUser
	}
	// Retrieve the user's entries using the Ethereum address
	return a.EntriesByAddress(profile.Addr, timeout)
}

// Entry represents all the known information about an Akasha entry. The reason
// beind the nullable strings is to allow signalling unreachable IPFS content.
type Entry struct {
	ID        common.Hash    `json:"id"`
	Title     *string        `json:"title"`
	Author    common.Address `json:"author"`
	Published time.Time      `json:"published"`
	Tags      *[]string      `json:"tags"`
	Version   *int           `json:"version"`
	Comments  uint64         `json:"comments"`
	Content   *[]Block       `json:"content"`
}

// Entry retrieves all the details about a particular entry any user might have
// made.
func (a *akasha) Entry(hash common.Hash, timeout time.Duration) (*Entry, error) {
	it, err := a.entries.FilterPublish(nil, nil, [][32]byte{hash})
	if err != nil {
		return nil, err
	}
	defer it.Close()

	if !it.Next() {
		return nil, errUnknownEntry
	}
	return a.entry(it.Event, timeout)
}

// EntryByAddress retrieves all the details about a particular entry a user made,
// identified by the user's address and the entry id.
func (a *akasha) EntryByAddress(addr common.Address, hash common.Hash, timeout time.Duration) (*Entry, error) {
	it, err := a.entries.FilterPublish(nil, []common.Address{addr}, [][32]byte{hash})
	if err != nil {
		return nil, err
	}
	defer it.Close()

	if !it.Next() {
		return nil, errUnknownEntry
	}
	return a.entry(it.Event, timeout)
}

// EntryByName retrieves all the details about a particular entry a user made,
// identified by the user's name and the entry id.
func (a *akasha) EntryByName(name string, hash common.Hash, timeout time.Duration) (*Entry, error) {
	// Resolve the user's address from its ID
	var label [32]byte
	copy(label[:], name)

	node, err := a.registrar.Hash(nil, label)
	if err != nil {
		return nil, err
	}
	profile, err := a.resolver.Resolve(nil, node)
	if err != nil {
		return nil, err
	}
	if profile.Addr == (common.Address{}) {
		return nil, errUnknownUser
	}
	// Retrieve the user's entry using the Ethereum address
	return a.EntryByAddress(profile.Addr, hash, timeout)
}

// entry retrieves all the known details about an Akasha post based on the publish
// log from the Ethereum contract.
func (a *akasha) entry(event *contracts.EntriesPublish, timeout time.Duration) (*Entry, error) {
	// Resolve the IPFS id of the entry
	post, err := a.entries.GetEntry(nil, event.Author, event.EntryId)
	if err != nil {
		return nil, err
	}
	id := base58.Encode(append([]byte{post.Fn, post.DigestSize}, post.Hash[:]...))

	// Start assembling the entry with whatever data we can pull off IPFS
	header, err := a.eth.HeaderByHash(context.TODO(), event.Raw.BlockHash)
	if err != nil {
		return nil, err
	}
	comments, err := a.comments.TotalComments(nil, event.EntryId)
	if err != nil {
		return nil, err
	}
	entry := &Entry{
		ID:        event.EntryId,
		Author:    event.Author,
		Published: time.Unix(header.Time.Int64(), 0),
		Comments:  comments.Uint64(),
	}
	// Retrieve the components of the entry and resolve the individual contents
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	blob, err := a.ipfs.Content(ctx, id)
	if err == nil {
		// Entry metadata retrieved, parse the primary details
		var metadata struct {
			Title   string   `json:"title"`
			Tags    []string `json:"tags"`
			Version int      `json:"version"`
			Parts   int      `json:"draftParts"`
		}
		if err = json.Unmarshal(blob, &metadata); err != nil {
			return nil, err
		}
		entry.Title = &metadata.Title
		entry.Tags = &metadata.Tags
		entry.Version = &metadata.Version

		objs, err := a.ipfs.Links(ctx, id)
		if err == nil {
			// Accumulate the individual parts of the entry
			var document []byte

			for i := 0; i < metadata.Parts; i++ {
				if blob, err = a.ipfs.Content(ctx, objs[fmt.Sprintf("draft-part%d", i)]); err != nil {
					break
				}
				document = append(document, blob...)
			}
			select {
			case <-ctx.Done():
				return entry, nil
			default:
			}
			// Parse the document and create the list of data blocks
			content, err := parseDraftjs(document)
			if err != nil {
				return nil, err
			}
			entry.Content = &content
		}
	}
	return entry, nil
}

// CommentsByAddress retrieves a list of comments by a user given its Ethereum
// address.
func (a *akasha) CommentsByAddress(addr common.Address, timeout time.Duration) ([]common.Hash, error) {
	// Filter the Ethereum events for Akasha comment publishes
	it, err := a.comments.FilterPublish(nil, []common.Address{addr}, nil, nil)
	if err != nil {
		return nil, err
	}
	defer it.Close()

	// Gather all the entry ids and return them to the user
	var ids []common.Hash
	for it.Next() {
		ids = append(ids, it.Event.Id)
	}
	return ids, nil
}

// CommentsByName retrieves a list of comments posted by a user given its Akasha
// username.
func (a *akasha) CommentsByName(name string, timeout time.Duration) ([]common.Hash, error) {
	// Resolve the user's address from its ID
	var label [32]byte
	copy(label[:], name)

	node, err := a.registrar.Hash(nil, label)
	if err != nil {
		return nil, err
	}
	profile, err := a.resolver.Resolve(nil, node)
	if err != nil {
		return nil, err
	}
	if profile.Addr == (common.Address{}) {
		return nil, errUnknownUser
	}
	// Retrieve the user's comments using the Ethereum address
	return a.CommentsByAddress(profile.Addr, timeout)
}

// CommentsByEntry retrieves a list of comments posted on an Akasha entry.
func (a *akasha) CommentsByEntry(entry common.Hash, timeout time.Duration) ([]common.Hash, error) {
	// Filter the Ethereum events for Akasha comment publishes
	it, err := a.comments.FilterPublish(nil, nil, [][32]byte{entry}, nil)
	if err != nil {
		return nil, err
	}
	defer it.Close()

	// Gather all the entry ids and return them to the user
	var ids []common.Hash
	for it.Next() {
		ids = append(ids, it.Event.Id)
	}
	return ids, nil
}

// Comment represents all the known information about an Akasha entry comment.
// The reason beind the nullable strings is to allow signalling unreachable
// IPFS content.
type Comment struct {
	ID        common.Hash    `json:"id"`
	Author    common.Address `json:"author"`
	Entry     common.Hash    `json:"entry"`
	Published time.Time      `json:"published"`
	Content   *[]Block       `json:"content"`
}

// CommentsByAddress retrieves all the known details about an Akasha post comment.
func (a *akasha) CommentByAddress(addr common.Address, hash common.Hash, timeout time.Duration) (*Comment, error) {
	// Filter the Ethereum events for Akasha comment publishes
	it, err := a.comments.FilterPublish(nil, []common.Address{addr}, nil, nil)
	if err != nil {
		return nil, err
	}
	defer it.Close()

	// Find the particular comment requested
	for it.Next() {
		if it.Event.Id == hash {
			return a.comment(it.Event, timeout)
		}
	}
	return nil, errUnknownComment
}

// CommentsByName retrieves all the known details about an Akasha post comment.
func (a *akasha) CommentByName(name string, hash common.Hash, timeout time.Duration) (*Comment, error) {
	// Resolve the user's address from its ID
	var label [32]byte
	copy(label[:], name)

	node, err := a.registrar.Hash(nil, label)
	if err != nil {
		return nil, err
	}
	profile, err := a.resolver.Resolve(nil, node)
	if err != nil {
		return nil, err
	}
	if profile.Addr == (common.Address{}) {
		return nil, errUnknownUser
	}
	// Retrieve the user's comment using the Ethereum address
	return a.CommentByAddress(profile.Addr, hash, timeout)
}

// CommentByEntry retrieves all the known details about an Akasha post comment.
func (a *akasha) CommentByEntry(entry common.Hash, hash common.Hash, timeout time.Duration) (*Comment, error) {
	// Filter for all the comments of a particular entry
	it, err := a.comments.FilterPublish(nil, nil, [][32]byte{entry}, nil)
	if err != nil {
		return nil, err
	}
	defer it.Close()

	// Find the particular comment requested
	for it.Next() {
		if it.Event.Id == hash {
			return a.comment(it.Event, timeout)
		}
	}
	return nil, errUnknownComment
}

// comment retrieves all the known details about an Akasha post comment based on
// the publish log from the Ethereum contract.
func (a *akasha) comment(event *contracts.CommentsPublish, timeout time.Duration) (*Comment, error) {
	// Resolve the IPFS id of the comment
	post, err := a.comments.GetComment(nil, event.EntryId, event.Id)
	if err != nil {
		return nil, err
	}
	id := base58.Encode(append([]byte{post.Fn, post.DigestSize}, post.Hash[:]...))

	// Start assembling the comment with whatever data we can pull off IPFS
	header, err := a.eth.HeaderByHash(context.TODO(), event.Raw.BlockHash)
	if err != nil {
		return nil, err
	}
	comment := &Comment{
		ID:        event.Id,
		Author:    event.Author,
		Entry:     event.EntryId,
		Published: time.Unix(header.Time.Int64(), 0),
	}
	// Retrieve the content of the comment
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	blob, err := a.ipfs.Content(ctx, id)
	if err == nil {
		// Comment metadata retrieved, extract the comment content
		var metadata struct {
			Content string `json:"content"`
		}
		if err = json.Unmarshal(blob, &metadata); err != nil {
			return nil, err
		}
		// Parse the document and create the list of data blocks
		content, err := parseDraftjs([]byte(metadata.Content))
		if err != nil {
			return nil, err
		}
		comment.Content = &content
	}
	return comment, nil
}

// Prefetch starts a background process to monitor the Ethereum chain for Akasha
// contract events and prefetch any IPFS resources when they are published.
func (a *akasha) Prefetch() error {
	// Start a prefetcher for entry publishes
	entryPubs := make(chan *contracts.EntriesPublish, 128)
	commentPubs := make(chan *contracts.CommentsPublish, 128)

	entrySub, err := a.entries.WatchPublish(nil, entryPubs, nil, nil)
	if err != nil {
		return err
	}
	commentSub, err := a.comments.WatchPublish(nil, commentPubs, nil, nil, nil)
	if err != nil {
		return err
	}
	go func() {
		defer entrySub.Unsubscribe()
		defer commentSub.Unsubscribe()

		for {
			select {
			case event := <-entryPubs:
				// Notification arrived for new entry in Akasha, prefetch it
				log.Info("Prefetching new entry", "author", event.Author, "entry", common.Hash(event.EntryId))

				go func() {
					entry, err := a.entry(event, 15*time.Second)
					switch {
					case err != nil:
						log.Error("Failed to prefetch published entry", "author", event.Author, "entry", common.Hash(event.EntryId), "err", err)
					case entry.Content == nil:
						log.Warn("Failed to prefetch published entry", "author", event.Author, "entry", common.Hash(event.EntryId))
					default:
						log.Info("Prefetched new entry", "author", event.Author, "entry", common.Hash(event.EntryId), "title", *entry.Title)
					}
				}()
			case event := <-commentPubs:
				// Notification arrived for new comment in Akasha, prefetch it
				log.Info("Prefetching new comment", "author", event.Author, "entry", common.Hash(event.EntryId), "comment", common.Hash(event.Id))

				go func() {
					entry, err := a.comment(event, 15*time.Second)
					switch {
					case err != nil:
						log.Error("Failed to prefetch published comment", "author", event.Author, "entry", common.Hash(event.EntryId), "comment", common.Hash(event.Id), "err", err)
					case entry.Content == nil:
						log.Warn("Failed to prefetch published comment", "author", event.Author, "entry", common.Hash(event.EntryId), "comment", common.Hash(event.Id))
					default:
						log.Info("Prefetched new comment", "author", event.Author, "entry", common.Hash(event.EntryId), "comment", common.Hash(event.Id))
					}
				}()
			case err := <-entrySub.Err():
				log.Error("Entry publish event watch failed: %v", err)
				return
			case err := <-entrySub.Err():
				log.Error("Comment publish event watch failed: %v", err)
				return
			}
		}
	}()
	return nil
}
