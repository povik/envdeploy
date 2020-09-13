package main

import (
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

func initNet() {
}

func createRouteFromCurrentNetns(hostIfName string) error {
	currentNs, err := netns.Get()
	if err != nil {
		return err
	}

	return runInBackgroundNetns(func() error {
		la := netlink.NewLinkAttrs()
		la.Name = "eth1"
		la.Namespace = netlink.NsFd(currentNs)

		veth := &netlink.Veth{
			LinkAttrs: la,
			PeerName:  hostIfName,
		}

		return netlink.LinkAdd(veth)
	})
}

func runInBackgroundNetns(payload func() error) error {
	/* If an OS thread has non-background net namespace, it must
	   have been locked to a goroutine.  Newly spawned goroutine
	   will always start in the background namespace. */

	finishedch := make(chan error)
	go func() {
		finishedch <- payload()
	}()
	return <-finishedch
}
