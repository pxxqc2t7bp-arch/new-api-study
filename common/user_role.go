package common

// CanManageUserRole reports whether an operator may manage a target account.
// Plugin administrators are isolated from ordinary administrators regardless
// of their numeric role ordering.
func CanManageUserRole(operatorRole, targetRole int) bool {
	if targetRole == RolePluginAdminUser {
		return operatorRole == RoleRootUser
	}
	return operatorRole == RoleRootUser || operatorRole > targetRole
}

// CanManageLowerUserRole applies the same policy to operations that also
// require the target role to be strictly lower than the operator role.
func CanManageLowerUserRole(operatorRole, targetRole int) bool {
	return operatorRole > targetRole && CanManageUserRole(operatorRole, targetRole)
}
