package account

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/argoproj/argo-cd/v3/common"
	"github.com/argoproj/argo-cd/v3/pkg/apiclient/account"
	sessionpkg "github.com/argoproj/argo-cd/v3/pkg/apiclient/session"
	"github.com/argoproj/argo-cd/v3/server/session"
	"github.com/argoproj/argo-cd/v3/test"
	"github.com/argoproj/argo-cd/v3/util/password"
	"github.com/argoproj/argo-cd/v3/util/rbac"
	sessionutil "github.com/argoproj/argo-cd/v3/util/session"
	"github.com/argoproj/argo-cd/v3/util/settings"
)

const (
	testNamespace = "default"
)

// return an AccountServer which returns fake data
func newTestAccountServer(t *testing.T, ctx context.Context, opts ...func(cm *corev1.ConfigMap, secret *corev1.Secret)) (*Server, *session.Server) {
	t.Helper()
	return newTestAccountServerExt(t, ctx, func(_ jwt.Claims, _ ...any) bool {
		return true
	}, opts...)
}

func newTestAccountServerExt(t *testing.T, ctx context.Context, enforceFn rbac.ClaimsEnforcerFunc, opts ...func(cm *corev1.ConfigMap, secret *corev1.Secret)) (*Server, *session.Server) {
	t.Helper()
	bcrypt, err := password.HashPassword("oldpassword")
	require.NoError(t, err)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "argocd-cm",
			Namespace: testNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/part-of": "argocd",
			},
		},
		Data: map[string]string{},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "argocd-secret",
			Namespace: testNamespace,
		},
		Data: map[string][]byte{
			"admin.password":   []byte(bcrypt),
			"server.secretkey": []byte("test"),
		},
	}
	for i := range opts {
		opts[i](cm, secret)
	}
	kubeclientset := fake.NewClientset(cm, secret)
	settingsMgr := settings.NewSettingsManager(ctx, kubeclientset, testNamespace)
	sessionMgr := sessionutil.NewSessionManager(settingsMgr, test.NewFakeProjLister(), "", nil, sessionutil.NewUserStateStorage(nil))
	enforcer := rbac.NewEnforcer(kubeclientset, testNamespace, common.ArgoCDRBACConfigMapName, nil)
	enforcer.SetClaimsEnforcerFunc(enforceFn)

	return NewServer(sessionMgr, settingsMgr, enforcer), session.NewServer(sessionMgr, settingsMgr, nil, nil, nil)
}

func getAdminAccount(mgr *settings.SettingsManager) (*settings.Account, error) {
	accounts, err := mgr.GetAccounts()
	if err != nil {
		return nil, err
	}
	adminAccount := accounts[common.ArgoCDAdminUsername]
	return &adminAccount, nil
}

func adminContext(ctx context.Context) context.Context {
	//nolint:staticcheck
	return context.WithValue(ctx, "claims", &jwt.RegisteredClaims{Subject: "admin", Issuer: sessionutil.SessionManagerClaimsIssuer})
}

func ssoAdminContext(ctx context.Context, iat time.Time) context.Context {
	//nolint:staticcheck
	return context.WithValue(ctx, "claims", &jwt.RegisteredClaims{
		Subject:  "admin",
		Issuer:   "https://myargocdhost.com/api/dex",
		IssuedAt: jwt.NewNumericDate(iat),
	})
}

func projTokenContext(ctx context.Context) context.Context {
	//nolint:staticcheck
	return context.WithValue(ctx, "claims", &jwt.RegisteredClaims{
		Subject: "proj:demo:deployer",
		Issuer:  sessionutil.SessionManagerClaimsIssuer,
	})
}

func TestUpdatePassword(t *testing.T) {
	accountServer, sessionServer := newTestAccountServer(t, t.Context())
	ctx := adminContext(t.Context())
	var err error

	// ensure password is not allowed to be updated if given bad password
	_, err = accountServer.UpdatePassword(ctx, &account.UpdatePasswordRequest{CurrentPassword: "badpassword", NewPassword: "newpassword"})
	require.Error(t, err)
	require.NoError(t, accountServer.sessionMgr.VerifyUsernamePassword("admin", "oldpassword"))
	require.Error(t, accountServer.sessionMgr.VerifyUsernamePassword("admin", "newpassword"))
	// verify old password works
	_, err = sessionServer.Create(ctx, &sessionpkg.SessionCreateRequest{Username: "admin", Password: "oldpassword"})
	require.NoError(t, err)
	// verify new password doesn't
	_, err = sessionServer.Create(ctx, &sessionpkg.SessionCreateRequest{Username: "admin", Password: "newpassword"})
	require.Error(t, err)

	// ensure password can be updated with valid password and immediately be used
	adminAccount, err := getAdminAccount(accountServer.settingsMgr)
	require.NoError(t, err)
	prevHash := adminAccount.PasswordHash
	_, err = accountServer.UpdatePassword(ctx, &account.UpdatePasswordRequest{CurrentPassword: "oldpassword", NewPassword: "newpassword"})
	require.NoError(t, err)
	adminAccount, err = getAdminAccount(accountServer.settingsMgr)
	require.NoError(t, err)
	assert.NotEqual(t, prevHash, adminAccount.PasswordHash)
	require.NoError(t, accountServer.sessionMgr.VerifyUsernamePassword("admin", "newpassword"))
	require.Error(t, accountServer.sessionMgr.VerifyUsernamePassword("admin", "oldpassword"))
	// verify old password is invalid
	_, err = sessionServer.Create(ctx, &sessionpkg.SessionCreateRequest{Username: "admin", Password: "oldpassword"})
	require.Error(t, err)
	// verify new password works
	_, err = sessionServer.Create(ctx, &sessionpkg.SessionCreateRequest{Username: "admin", Password: "newpassword"})
	require.NoError(t, err)
}

func TestUpdatePassword_AdminUpdatesAnotherUser(t *testing.T) {
	accountServer, sessionServer := newTestAccountServer(t, t.Context(), func(cm *corev1.ConfigMap, _ *corev1.Secret) {
		cm.Data["accounts.anotherUser"] = "login"
	})
	ctx := adminContext(t.Context())

	_, err := accountServer.UpdatePassword(ctx, &account.UpdatePasswordRequest{CurrentPassword: "oldpassword", NewPassword: "newpassword", Name: "anotherUser"})
	require.NoError(t, err)

	_, err = sessionServer.Create(ctx, &sessionpkg.SessionCreateRequest{Username: "anotherUser", Password: "newpassword"})
	require.NoError(t, err)
}

func TestUpdatePassword_DoesNotHavePermissions(t *testing.T) {
	enforcer := func(_ jwt.Claims, _ ...any) bool {
		return false
	}

	t.Run("LocalAccountUpdatesAnotherAccount", func(t *testing.T) {
		accountServer, _ := newTestAccountServerExt(t, t.Context(), enforcer, func(cm *corev1.ConfigMap, _ *corev1.Secret) {
			cm.Data["accounts.anotherUser"] = "login"
		})
		ctx := adminContext(t.Context())
		_, err := accountServer.UpdatePassword(ctx, &account.UpdatePasswordRequest{CurrentPassword: "oldpassword", NewPassword: "newpassword", Name: "anotherUser"})
		assert.ErrorContains(t, err, "permission denied")
	})

	t.Run("SSOAccountWithTheSameName", func(t *testing.T) {
		accountServer, _ := newTestAccountServerExt(t, t.Context(), enforcer)
		ctx := ssoAdminContext(t.Context(), time.Now())
		_, err := accountServer.UpdatePassword(ctx, &account.UpdatePasswordRequest{CurrentPassword: "oldpassword", NewPassword: "newpassword", Name: "admin"})
		assert.ErrorContains(t, err, "permission denied")
	})
}

func TestUpdatePassword_ProjectToken(t *testing.T) {
	accountServer, _ := newTestAccountServer(t, t.Context(), func(cm *corev1.ConfigMap, _ *corev1.Secret) {
		cm.Data["accounts.anotherUser"] = "login"
	})
	ctx := projTokenContext(t.Context())
	_, err := accountServer.UpdatePassword(ctx, &account.UpdatePasswordRequest{CurrentPassword: "oldpassword", NewPassword: "newpassword"})
	assert.ErrorContains(t, err, "password can only be changed for local users")
}

func TestUpdatePassword_OldSSOToken(t *testing.T) {
	accountServer, _ := newTestAccountServer(t, t.Context(), func(cm *corev1.ConfigMap, _ *corev1.Secret) {
		cm.Data["accounts.anotherUser"] = "login"
	})
	ctx := ssoAdminContext(t.Context(), time.Now().Add(-2*common.ChangePasswordSSOTokenMaxAge))

	_, err := accountServer.UpdatePassword(ctx, &account.UpdatePasswordRequest{CurrentPassword: "oldpassword", NewPassword: "newpassword", Name: "anotherUser"})
	require.Error(t, err)
}

func TestUpdatePassword_SSOUserUpdatesAnotherUser(t *testing.T) {
	accountServer, sessionServer := newTestAccountServer(t, t.Context(), func(cm *corev1.ConfigMap, _ *corev1.Secret) {
		cm.Data["accounts.anotherUser"] = "login"
	})
	ctx := ssoAdminContext(t.Context(), time.Now())

	_, err := accountServer.UpdatePassword(ctx, &account.UpdatePasswordRequest{CurrentPassword: "oldpassword", NewPassword: "newpassword", Name: "anotherUser"})
	require.NoError(t, err)

	_, err = sessionServer.Create(ctx, &sessionpkg.SessionCreateRequest{Username: "anotherUser", Password: "newpassword"})
	require.NoError(t, err)
}

func TestListAccounts_NoAccountsConfigured(t *testing.T) {
	ctx := adminContext(t.Context())

	accountServer, _ := newTestAccountServer(t, ctx)
	resp, err := accountServer.ListAccounts(ctx, &account.ListAccountRequest{})
	require.NoError(t, err)
	assert.Len(t, resp.Items, 1)
}

func TestListAccounts_AccountsAreConfigured(t *testing.T) {
	ctx := adminContext(t.Context())
	accountServer, _ := newTestAccountServer(t, ctx, func(cm *corev1.ConfigMap, _ *corev1.Secret) {
		cm.Data["accounts.account1"] = "apiKey"
		cm.Data["accounts.account2"] = "login, apiKey"
		cm.Data["accounts.account2.enabled"] = "false"
	})

	resp, err := accountServer.ListAccounts(ctx, &account.ListAccountRequest{})
	require.NoError(t, err)
	assert.Len(t, resp.Items, 3)
	assert.ElementsMatch(t, []*account.Account{
		{Name: "admin", Capabilities: []string{"login"}, Enabled: true},
		{Name: "account1", Capabilities: []string{"apiKey"}, Enabled: true},
		{Name: "account2", Capabilities: []string{"login", "apiKey"}, Enabled: false},
	}, resp.Items)
}

func TestGetAccount(t *testing.T) {
	ctx := adminContext(t.Context())
	accountServer, _ := newTestAccountServer(t, ctx, func(cm *corev1.ConfigMap, _ *corev1.Secret) {
		cm.Data["accounts.account1"] = "apiKey"
	})

	t.Run("ExistingAccount", func(t *testing.T) {
		acc, err := accountServer.GetAccount(ctx, &account.GetAccountRequest{Name: "account1"})
		require.NoError(t, err)

		assert.Equal(t, "account1", acc.Name)
	})

	t.Run("NonExistingAccount", func(t *testing.T) {
		_, err := accountServer.GetAccount(ctx, &account.GetAccountRequest{Name: "bad-name"})
		require.Error(t, err)
		assert.Equal(t, codes.NotFound, status.Code(err))
	})
}

func TestCreateToken_SuccessfullyCreated(t *testing.T) {
	ctx := adminContext(t.Context())
	accountServer, _ := newTestAccountServer(t, ctx, func(cm *corev1.ConfigMap, _ *corev1.Secret) {
		cm.Data["accounts.account1"] = "apiKey"
	})

	_, err := accountServer.CreateToken(ctx, &account.CreateTokenRequest{Name: "account1"})
	require.NoError(t, err)

	acc, err := accountServer.GetAccount(ctx, &account.GetAccountRequest{Name: "account1"})
	require.NoError(t, err)

	assert.Len(t, acc.Tokens, 1)
}

func TestCreateToken_DoesNotHaveCapability(t *testing.T) {
	ctx := adminContext(t.Context())
	accountServer, _ := newTestAccountServer(t, ctx, func(cm *corev1.ConfigMap, _ *corev1.Secret) {
		cm.Data["accounts.account1"] = "login"
	})

	_, err := accountServer.CreateToken(ctx, &account.CreateTokenRequest{Name: "account1"})
	require.Error(t, err)
}

func TestCreateToken_UserSpecifiedID(t *testing.T) {
	ctx := adminContext(t.Context())
	accountServer, _ := newTestAccountServer(t, ctx, func(cm *corev1.ConfigMap, _ *corev1.Secret) {
		cm.Data["accounts.account1"] = "apiKey"
	})

	_, err := accountServer.CreateToken(ctx, &account.CreateTokenRequest{Name: "account1", Id: "test"})
	require.NoError(t, err)

	_, err = accountServer.CreateToken(ctx, &account.CreateTokenRequest{Name: "account1", Id: "test"})
	require.ErrorContains(t, err, "failed to update account with new token:")
	assert.ErrorContains(t, err, "account already has token with id 'test'")
}

func TestDeleteToken_SuccessfullyRemoved(t *testing.T) {
	ctx := adminContext(t.Context())
	accountServer, _ := newTestAccountServer(t, ctx, func(cm *corev1.ConfigMap, secret *corev1.Secret) {
		cm.Data["accounts.account1"] = "apiKey"
		secret.Data["accounts.account1.tokens"] = []byte(`[{"id":"123","iat":1583789194,"exp":1583789194}]`)
	})

	_, err := accountServer.DeleteToken(ctx, &account.DeleteTokenRequest{Name: "account1", Id: "123"})
	require.NoError(t, err)

	acc, err := accountServer.GetAccount(ctx, &account.GetAccountRequest{Name: "account1"})
	require.NoError(t, err)

	assert.Empty(t, acc.Tokens)
}

func TestCanI_GetLogsAllow(t *testing.T) {
	accountServer, _ := newTestAccountServer(t, t.Context(), func(_ *corev1.ConfigMap, _ *corev1.Secret) {
	})

	ctx := projTokenContext(t.Context())
	resp, err := accountServer.CanI(ctx, &account.CanIRequest{Resource: "logs", Action: "get", Subresource: ""})
	require.NoError(t, err)
	assert.Equal(t, "yes", resp.Value)
}

func TestCanI_GetLogsDeny(t *testing.T) {
	enforcer := func(_ jwt.Claims, _ ...any) bool {
		return false
	}

	accountServer, _ := newTestAccountServerExt(t, t.Context(), enforcer, func(_ *corev1.ConfigMap, _ *corev1.Secret) {
	})

	ctx := projTokenContext(t.Context())
	resp, err := accountServer.CanI(ctx, &account.CanIRequest{Resource: "logs", Action: "get", Subresource: "*/*"})
	require.NoError(t, err)
	assert.Equal(t, "no", resp.Value)
}
