// DEBUG kubectl tools
// 1、fmt.Printf("%#v")  print type and value
// 2、panic()  function invoke stack

package main

import (
	"fmt"
	"testing"
)

type I interface {
	test()
}

type S struct {
	a int
}

func (s *S) test() {
	panic("test")
}

func Test_Main(t *testing.T) {
	a := "xdfd"
	b := &a
	c := &b
	fmt.Println(c)
	fmt.Printf("%#v\n", *c)

	var i I
	i = &S{
		a: 4,
	}

	i.test()

	fmt.Printf("%#v\n", i)
}
