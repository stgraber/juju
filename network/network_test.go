// Copyright 2014 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package network_test

import (
	"errors"
	"io/ioutil"
	"net"
	"path/filepath"

	jc "github.com/juju/testing/checkers"
	"github.com/juju/utils/set"
	gc "gopkg.in/check.v1"

	"github.com/juju/juju/network"
	"github.com/juju/juju/testing"
)

type InterfaceInfoSuite struct {
	info []network.InterfaceInfo
}

var _ = gc.Suite(&InterfaceInfoSuite{})

func (s *InterfaceInfoSuite) SetUpTest(c *gc.C) {
	s.info = []network.InterfaceInfo{
		{VLANTag: 1, DeviceIndex: 0, InterfaceName: "eth0"},
		{VLANTag: 0, DeviceIndex: 1, InterfaceName: "eth1"},
		{VLANTag: 42, DeviceIndex: 2, InterfaceName: "br2"},
		{ConfigType: network.ConfigDHCP, NoAutoStart: true},
		{Address: network.NewAddress("0.1.2.3")},
		{DNSServers: network.NewAddresses("1.1.1.1", "2.2.2.2")},
		{GatewayAddress: network.NewAddress("4.3.2.1")},
		{AvailabilityZones: []string{"foo", "bar"}},
	}
}

func (s *InterfaceInfoSuite) TestActualInterfaceName(c *gc.C) {
	c.Check(s.info[0].ActualInterfaceName(), gc.Equals, "eth0.1")
	c.Check(s.info[1].ActualInterfaceName(), gc.Equals, "eth1")
	c.Check(s.info[2].ActualInterfaceName(), gc.Equals, "br2.42")
}

func (s *InterfaceInfoSuite) TestIsVirtual(c *gc.C) {
	c.Check(s.info[0].IsVirtual(), jc.IsTrue)
	c.Check(s.info[1].IsVirtual(), jc.IsFalse)
	c.Check(s.info[2].IsVirtual(), jc.IsTrue)
}

func (s *InterfaceInfoSuite) TestIsVLAN(c *gc.C) {
	c.Check(s.info[0].IsVLAN(), jc.IsTrue)
	c.Check(s.info[1].IsVLAN(), jc.IsFalse)
	c.Check(s.info[2].IsVLAN(), jc.IsTrue)
}

func (s *InterfaceInfoSuite) TestAdditionalFields(c *gc.C) {
	c.Check(s.info[3].ConfigType, gc.Equals, network.ConfigDHCP)
	c.Check(s.info[3].NoAutoStart, jc.IsTrue)
	c.Check(s.info[4].Address, jc.DeepEquals, network.NewAddress("0.1.2.3"))
	c.Check(s.info[5].DNSServers, jc.DeepEquals, network.NewAddresses("1.1.1.1", "2.2.2.2"))
	c.Check(s.info[6].GatewayAddress, jc.DeepEquals, network.NewAddress("4.3.2.1"))
	c.Check(s.info[7].AvailabilityZones, jc.DeepEquals, []string{"foo", "bar"})
}

func (s *InterfaceInfoSuite) TestSortInterfaceInfo(c *gc.C) {
	info := []network.InterfaceInfo{
		{VLANTag: 42, DeviceIndex: 2, InterfaceName: "br2"},
		{VLANTag: 0, DeviceIndex: 1, InterfaceName: "eth1"},
		{VLANTag: 1, DeviceIndex: 0, InterfaceName: "eth0"},
	}
	expectedInfo := []network.InterfaceInfo{
		{VLANTag: 1, DeviceIndex: 0, InterfaceName: "eth0"},
		{VLANTag: 0, DeviceIndex: 1, InterfaceName: "eth1"},
		{VLANTag: 42, DeviceIndex: 2, InterfaceName: "br2"},
	}
	network.SortInterfaceInfo(info)
	c.Assert(info, jc.DeepEquals, expectedInfo)
}

type NetworkSuite struct {
	testing.BaseSuite
}

var _ = gc.Suite(&NetworkSuite{})

func (s *NetworkSuite) TestConvertSpaceName(c *gc.C) {
	empty := set.Strings{}
	nameTests := []struct {
		name     string
		existing set.Strings
		expected string
	}{
		{"foo", empty, "foo"},
		{"foo1", empty, "foo1"},
		{"Foo Thing", empty, "foo-thing"},
		{"foo^9*//++!!!!", empty, "foo9"},
		{"--Foo", empty, "foo"},
		{"---^^&*()!", empty, "empty"},
		{" ", empty, "empty"},
		{"", empty, "empty"},
		{"foo\u2318", empty, "foo"},
		{"foo--", empty, "foo"},
		{"-foo--foo----bar-", empty, "foo-foo-bar"},
		{"foo-", set.NewStrings("foo", "bar", "baz"), "foo-2"},
		{"foo", set.NewStrings("foo", "foo-2"), "foo-3"},
		{"---", set.NewStrings("empty"), "empty-2"},
	}
	for _, test := range nameTests {
		result := network.ConvertSpaceName(test.name, test.existing)
		c.Check(result, gc.Equals, test.expected)
	}
}

func (s *NetworkSuite) TestFilterBridgeAddresses(c *gc.C) {
	lxcFakeNetConfig := filepath.Join(c.MkDir(), "lxc-net")
	// We create an LXC bridge named "foobar", and then put 10.0.3.1,
	// 10.0.3.4 and 10.0.3.5/24 on that bridge.
	// We also put 10.0.4.1 and 10.0.5.1/24 onto whatever bridge LXD is
	// configured to use.
	// And 192.168.122.1 on virbr0
	netConf := []byte(`
  # comments ignored
LXC_BR= ignored
LXC_ADDR = "fooo"
 LXC_BRIDGE = " foobar " # detected, spaces stripped
anything else ignored
LXC_BRIDGE="ignored"`[1:])
	err := ioutil.WriteFile(lxcFakeNetConfig, netConf, 0644)
	c.Assert(err, jc.ErrorIsNil)
	s.PatchValue(&network.InterfaceByNameAddrs, func(name string) ([]net.Addr, error) {
		if name == "foobar" {
			return []net.Addr{
				&net.IPAddr{IP: net.IPv4(10, 0, 3, 1)},
				&net.IPAddr{IP: net.IPv4(10, 0, 3, 4)},
				// Try a CIDR 10.0.3.5/24 as well.
				&net.IPNet{IP: net.IPv4(10, 0, 3, 5), Mask: net.IPv4Mask(255, 255, 255, 0)},
			}, nil
		} else if name == network.DefaultLXDBridge {
			return []net.Addr{
				&net.IPAddr{IP: net.IPv4(10, 0, 4, 1)},
				// Try a CIDR 10.0.5.1/24 as well.
				&net.IPNet{IP: net.IPv4(10, 0, 5, 1), Mask: net.IPv4Mask(255, 255, 255, 0)},
			}, nil
		} else if name == network.DefaultKVMBridge {
			return []net.Addr{
				&net.IPAddr{IP: net.IPv4(192, 168, 122, 1)},
			}, nil
		}
		c.Fatalf("unknown bridge name: %q", name)
		return nil, nil
	})
	s.PatchValue(&network.LXCNetDefaultConfig, lxcFakeNetConfig)

	inputAddresses := network.NewAddresses(
		"127.0.0.1",
		"2001:db8::1",
		"10.0.0.1",
		"10.0.3.1",      // filtered (directly as IP)
		"10.0.3.3",      // filtered (by the 10.0.3.5/24 CIDR)
		"10.0.3.5",      // filtered (directly)
		"10.0.3.4",      // filtered (directly)
		"10.0.4.1",      // filtered (directly from LXD bridge)
		"10.0.5.10",     // filtered (from LXD bridge, 10.0.5.1/24)
		"10.0.6.10",     // unfiltered
		"192.168.122.1", // filtered (from virbr0 bridge, 192.168.122.1)
		"192.168.123.42",
		"localhost", // unfiltered because it isn't an IP address
	)
	filteredAddresses := network.NewAddresses(
		"127.0.0.1",
		"2001:db8::1",
		"10.0.0.1",
		"10.0.6.10",
		"192.168.123.42",
		"localhost",
	)
	c.Assert(network.FilterBridgeAddresses(inputAddresses), jc.DeepEquals, filteredAddresses)
}

func (s *NetworkSuite) TestNoAddressError(c *gc.C) {
	err := network.NoAddressError("fake")
	c.Assert(err, gc.ErrorMatches, `no fake address\(es\)`)
	c.Assert(network.IsNoAddressError(err), jc.IsTrue)
	c.Assert(network.IsNoAddressError(errors.New("address found")), jc.IsFalse)
}
