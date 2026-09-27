export function authPageState(mode, setupRequired) {
  return {
    showCreateOwnerLink: mode === 'login' && setupRequired === true,
    redirectSignupToLogin: mode === 'signup' && setupRequired === false,
  };
}
