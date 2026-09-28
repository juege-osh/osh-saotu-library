package main

import (
	"net/http"
)

func (s *server) requireLevel(level int, next func(http.ResponseWriter, *http.Request, *currentUser)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, err := s.currentUser(r)
		if err != nil {
			apiError(w, http.StatusUnauthorized, "请先登录")
			return
		}
		if user.Level < level {
			apiError(w, http.StatusForbidden, "权限不足")
			return
		}
		next(w, r, user)
	}
}
