//go:build !linux && !windows

package service

import manager "github.com/kardianos/service"

func platformController(svc manager.Service) Controller { return svc }
