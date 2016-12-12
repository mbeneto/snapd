// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2016 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package snapstate

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"gopkg.in/tomb.v2"

	"github.com/snapcore/snapd/i18n/dumb"
	"github.com/snapcore/snapd/overlord/snapstate/backend"
	"github.com/snapcore/snapd/overlord/state"
	"github.com/snapcore/snapd/snap"
)

func getAliases(st *state.State, snapName string) (map[string]string, error) {
	var allAliases map[string]*json.RawMessage
	err := st.Get("aliases", &allAliases)
	if err != nil {
		return nil, err
	}
	raw := allAliases[snapName]
	if raw == nil {
		return nil, state.ErrNoState
	}
	var aliases map[string]string
	err = json.Unmarshal([]byte(*raw), &aliases)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal snap aliases state: %v", err)
	}
	return aliases, nil
}

func setAliases(st *state.State, snapName string, aliases map[string]string) {
	var allAliases map[string]*json.RawMessage
	err := st.Get("aliases", &allAliases)
	if err != nil && err != state.ErrNoState {
		panic("internal error: cannot unmarshal snap aliases state: " + err.Error())
	}
	if allAliases == nil {
		allAliases = make(map[string]*json.RawMessage)
	}
	if len(aliases) == 0 {
		delete(allAliases, snapName)
	} else {
		data, err := json.Marshal(aliases)
		if err != nil {
			panic("internal error: cannot marshal snap aliases state: " + err.Error())
		}
		raw := json.RawMessage(data)
		allAliases[snapName] = &raw
	}
	st.Set("aliases", allAliases)
}

// Alias enables the provided aliases for the snap with the given name.
func Alias(st *state.State, snapName string, aliases []string) (*state.TaskSet, error) {
	var snapst SnapState
	err := Get(st, snapName, &snapst)
	if err == state.ErrNoState {
		return nil, fmt.Errorf("cannot find snap %q", snapName)
	}
	if err != nil {
		return nil, err
	}
	if !snapst.Active {
		return nil, fmt.Errorf("enabling aliases for disabled snap %q not supported", snapName)
	}
	if err := checkChangeConflict(st, snapName, nil); err != nil {
		return nil, err
	}

	snapsup := &SnapSetup{
		SideInfo: &snap.SideInfo{RealName: snapName},
	}

	alias := st.NewTask("alias", fmt.Sprintf(i18n.G("Enable aliases for snap %q"), snapsup.Name()))
	alias.Set("snap-setup", &snapsup)
	toEnable := map[string]string{}
	for _, alias := range aliases {
		toEnable[alias] = "enabled"
	}
	alias.Set("aliases", toEnable)

	return state.NewTaskSet(alias), nil
}

func (m *SnapManager) doAlias(t *state.Task, _ *tomb.Tomb) error {
	st := t.State()
	st.Lock()
	defer st.Unlock()
	snapsup, snapst, err := snapSetupAndState(t)
	if err != nil {
		return err
	}
	var toEnable map[string]string
	err = t.Get("aliases", &toEnable)
	if err != nil {
		return err
	}
	snapName := snapsup.Name()
	curInfo, err := snapst.CurrentInfo()
	if err != nil {
		return err
	}
	aliasStatuses, err := getAliases(st, snapName)
	if err != nil && err != state.ErrNoState {
		return err
	}
	t.Set("old-aliases", aliasStatuses)
	if aliasStatuses == nil {
		aliasStatuses = make(map[string]string)
	}
	var add []*backend.Alias
	for alias := range toEnable {
		aliasApp := curInfo.Aliases[alias]
		if aliasApp == nil {
			return fmt.Errorf("cannot enable alias %q for %q, no such alias", alias, snapName)
		}
		if aliasStatuses[alias] == "enabled" {
			// nothing to do
			continue
		}
		err := checkAliasConflict(st, snapName, alias)
		if err != nil {
			return err
		}
		aliasStatuses[alias] = "enabled"
		add = append(add, &backend.Alias{
			Name:   alias,
			Target: filepath.Base(aliasApp.WrapperPath()),
		})
	}
	st.Unlock()
	err = m.backend.UpdateAliases(add, nil)
	st.Lock()
	if err != nil {
		return err
	}
	setAliases(st, snapName, aliasStatuses)
	return nil
}

func (m *SnapManager) undoAlias(t *state.Task, _ *tomb.Tomb) error {
	st := t.State()
	st.Lock()
	defer st.Unlock()
	var oldStatuses map[string]string
	err := t.Get("old-aliases", &oldStatuses)
	if err != nil {
		return err
	}
	snapsup, snapst, err := snapSetupAndState(t)
	if err != nil {
		return err
	}
	var toEnable map[string]string
	err = t.Get("aliases", &toEnable)
	if err != nil {
		return err
	}
	snapName := snapsup.Name()
	curInfo, err := snapst.CurrentInfo()
	if err != nil {
		return err
	}
	var remove []*backend.Alias
	for alias := range toEnable {
		if oldStatuses[alias] == "enabled" {
			// nothing to undo
			continue
		}
		aliasApp := curInfo.Aliases[alias]
		if aliasApp == nil {
			// unexpected
			return fmt.Errorf("internal error: cannot re-disable alias %q for %q, no such alias", alias, snapName)
		}
		remove = append(remove, &backend.Alias{
			Name:   alias,
			Target: filepath.Base(aliasApp.WrapperPath()),
		})
	}
	st.Unlock()
	remove, err = m.backend.MatchingAliases(remove)
	st.Lock()
	if err != nil {
		return fmt.Errorf("cannot list aliases for snap %q: %v", snapName, err)
	}
	st.Unlock()
	err = m.backend.UpdateAliases(nil, remove)
	st.Lock()
	if err != nil {
		return err
	}
	setAliases(st, snapName, oldStatuses)
	return nil

}

func (m *SnapManager) doClearAliases(t *state.Task, _ *tomb.Tomb) error {
	st := t.State()
	st.Lock()
	defer st.Unlock()
	snapsup, _, err := snapSetupAndState(t)
	if err != nil {
		return err
	}
	snapName := snapsup.Name()
	aliasStatuses, err := getAliases(st, snapName)
	if err != nil && err != state.ErrNoState {
		return err
	}
	if len(aliasStatuses) == 0 {
		// nothing to do
		return nil
	}
	t.Set("old-aliases", aliasStatuses)
	setAliases(st, snapName, nil)
	return nil
}

func (m *SnapManager) undoClearAliases(t *state.Task, _ *tomb.Tomb) error {
	st := t.State()
	st.Lock()
	defer st.Unlock()
	var oldStatuses map[string]string
	err := t.Get("old-aliases", &oldStatuses)
	if err == state.ErrNoState {
		// nothing to do
		return nil
	}
	if err != nil {
		return err
	}
	snapsup, _, err := snapSetupAndState(t)
	if err != nil {
		return err
	}
	snapName := snapsup.Name()

	for alias, status := range oldStatuses {
		if status == "enabled" {
			// can actually be reinstated only if it doesn't conflict
			err := checkAliasConflict(st, snapName, alias)
			if err != nil {
				if _, ok := err.(*aliasConflictError); ok {
					delete(oldStatuses, alias)
					t.Errorf("%v", err)
					continue
				}
				return err
			}
		}
	}
	setAliases(st, snapName, oldStatuses)
	return nil
}

func (m *SnapManager) doSetupAliases(t *state.Task, _ *tomb.Tomb) error {
	st := t.State()
	st.Lock()
	defer st.Unlock()
	snapsup, snapst, err := snapSetupAndState(t)
	if err != nil {
		return err
	}
	snapName := snapsup.Name()
	curInfo, err := snapst.CurrentInfo()
	if err != nil {
		return err
	}
	aliasStatuses, err := getAliases(st, snapName)
	if err != nil && err != state.ErrNoState {
		return err
	}
	var aliases []*backend.Alias
	for alias, aliasStatus := range aliasStatuses {
		if aliasStatus == "enabled" {
			aliasApp := curInfo.Aliases[alias]
			if aliasApp == nil {
				// not a known alias anymore, skip
				continue
			}
			aliases = append(aliases, &backend.Alias{
				Name:   alias,
				Target: filepath.Base(aliasApp.WrapperPath()),
			})
		}
	}
	st.Unlock()
	defer st.Lock()
	return m.backend.UpdateAliases(aliases, nil)
}

func (m *SnapManager) undoSetupAliases(t *state.Task, _ *tomb.Tomb) error {
	st := t.State()
	st.Lock()
	defer st.Unlock()
	snapsup, snapst, err := snapSetupAndState(t)
	if err != nil {
		return err
	}
	snapName := snapsup.Name()
	curInfo, err := snapst.CurrentInfo()
	if err != nil {
		return err
	}
	aliasStatuses, err := getAliases(st, snapName)
	if err != nil && err != state.ErrNoState {
		return err
	}
	var aliases []*backend.Alias
	for alias, aliasStatus := range aliasStatuses {
		if aliasStatus == "enabled" {
			aliasApp := curInfo.Aliases[alias]
			if aliasApp == nil {
				// not a known alias, skip
				continue
			}
			aliases = append(aliases, &backend.Alias{
				Name:   alias,
				Target: filepath.Base(aliasApp.WrapperPath()),
			})
		}
	}
	st.Unlock()
	rmAliases, err := m.backend.MatchingAliases(aliases)
	st.Lock()
	if err != nil {
		return fmt.Errorf("cannot list aliases for snap %q: %v", snapName, err)
	}
	st.Unlock()
	defer st.Lock()
	return m.backend.UpdateAliases(nil, rmAliases)
}

func (m *SnapManager) doRemoveAliases(t *state.Task, _ *tomb.Tomb) error {
	st := t.State()
	st.Lock()
	defer st.Unlock()
	snapsup, _, err := snapSetupAndState(t)
	if err != nil {
		return err
	}
	snapName := snapsup.Name()
	st.Unlock()
	defer st.Lock()
	return m.backend.RemoveSnapAliases(snapName)
}

func checkAgainstEnabledAliases(st *state.State, checker func(alias, otherSnap string) error) error {
	var allAliases map[string]map[string]string
	err := st.Get("aliases", &allAliases)
	if err == state.ErrNoState {
		return nil
	}
	if err != nil {
		return err
	}
	for otherSnap, aliasStatuses := range allAliases {
		for alias, aliasStatus := range aliasStatuses {
			if aliasStatus == "enabled" {
				if err := checker(alias, otherSnap); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func checkSnapAliasConflict(st *state.State, snapName string) error {
	prefix := fmt.Sprintf("%s.", snapName)
	return checkAgainstEnabledAliases(st, func(alias, otherSnap string) error {
		if alias == snapName || strings.HasPrefix(alias, prefix) {
			return fmt.Errorf("snap %q command namespace conflicts with enabled alias %q for %q", snapName, alias, otherSnap)
		}
		return nil
	})
}

type aliasConflictError struct {
	Alias  string
	Snap   string
	Reason string
}

func (e *aliasConflictError) Error() string {
	return fmt.Sprintf("cannot enable alias %q for %q, %s", e.Alias, e.Snap, e.Reason)
}

func checkAliasConflict(st *state.State, snapName, alias string) error {
	// check against snaps
	var snapNames map[string]*json.RawMessage
	err := st.Get("snaps", &snapNames)
	if err != nil && err != state.ErrNoState {
		return err
	}
	for name := range snapNames {
		if name == alias || strings.HasPrefix(alias, name+".") {
			return &aliasConflictError{
				Alias:  alias,
				Snap:   snapName,
				Reason: fmt.Sprintf("it conflicts with the command namespace of installed snap %q", name),
			}
		}
	}

	// check against aliases
	return checkAgainstEnabledAliases(st, func(otherAlias, otherSnap string) error {
		if otherAlias == alias {
			return &aliasConflictError{
				Alias:  alias,
				Snap:   snapName,
				Reason: fmt.Sprintf("already enabled for %q", otherSnap),
			}
		}
		return nil
	})
}