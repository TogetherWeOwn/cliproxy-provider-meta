//go:build !hostcompat

package main

import "context"

func selectedEndpoint(endpoint string) string     { return endpoint }
func configureBase(context.Context, []byte) error { return nil }
