package main

import (
	"flag"
	"strings"
)

type user string

var (
	flagAdminUsers = flag.String("admins", "", "comma-separated list of admin usernames")
)

var isAdminMap map[user]bool

func initUsers() {
	isAdminMap = make(map[user]bool)
	for _, un := range strings.Split(*flagAdminUsers, ",") {
		isAdminMap[user(un)] = true
	}
}

func (u user) IsAdmin() bool {
	return isAdminMap[u]
}

func (u user) CanAccessJob(j *job) bool {
	return j.Public || u == j.Owner || u.IsAdmin()
}

func (u user) CanManageJob(j *job) bool {
	return u == j.Owner || u.IsAdmin()
}
