package main

import "testing"

func TestPasswordHash(t *testing.T) {
	h := hashPassword("correct horse")
	if !checkPassword(h, "correct horse") {
		t.Fatal("valid password rejected")
	}
	if checkPassword(h, "wrong") || checkPassword("garbage", "correct horse") {
		t.Fatal("invalid password accepted")
	}
}
