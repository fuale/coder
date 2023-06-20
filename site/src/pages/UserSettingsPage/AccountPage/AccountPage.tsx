import { FC } from "react"
import { Section } from "../../../components/SettingsLayout/Section"
import { AccountForm } from "../../../components/SettingsAccountForm/SettingsAccountForm"
import { useAuth } from "components/AuthProvider/AuthProvider"
import { useMe } from "hooks/useMe"
import { usePermissions } from "hooks/usePermissions"
import { SignInForm } from "components/SignInForm/SignInForm"
import { retrieveRedirect } from "utils/redirect"
import { useQuery } from "@tanstack/react-query"
import { convertToOauth, getAuthMethods } from "api/api"
import { AuthMethods } from "api/typesGenerated"
import axios from "axios"
import { Maybe } from "components/Conditionals/Maybe"

export const AccountPage: FC = () => {
  const queryKey = ["get-auth-methods"]
  const {
    data: authMethodsData,
    error: authMethodsError,
    isLoading: authMethodsLoading,
    isFetched: authMethodsFetched,
  } = useQuery({
    // select: (res: AuthMethods) => {
    //   return {
    //     ...res,
    //     // Disable the password auth in this account section. For merging accounts,
    //     // we only want to support oidc.
    //     password: {
    //       enabled: false,
    //     },
    //   }
    // },
    queryKey,
    queryFn: getAuthMethods,
  })

  const [authState, authSend] = useAuth()
  const me = useMe()
  const permissions = usePermissions()
  const { updateProfileError } = authState.context
  const canEditUsers = permissions && permissions.updateUsers
  // Extra
  const redirectTo = retrieveRedirect(location.search)
  console.log(authState.context.data, authMethodsError)

  return (
    <Section title="Account" description="Update your account info">
      <AccountForm
        editable={Boolean(canEditUsers)}
        email={me.email}
        updateProfileError={updateProfileError}
        isLoading={authState.matches("signedIn.profile.updatingProfile")}
        initialValues={{
          username: me.username,
        }}
        onSubmit={(data) => {
          authSend({
            type: "UPDATE_PROFILE",
            data,
          })
        }}
      />

      <Maybe condition={authMethodsData?.me_login_type === "password"}>
        <SignInForm
          authMethods={authMethodsData}
          redirectTo={redirectTo}
          isSigningIn={false}
          error={authMethodsError}
          onSubmit={async (credentials: {
            email: string
            password: string
          }) => {
            const mergeState = await convertToOauth(
              credentials.email,
              credentials.password,
              "oidc",
            )

            window.location.href = `/api/v2/users/oidc/callback?oidc_merge_state=${
              mergeState?.state_string
            }&redirect=${encodeURIComponent(redirectTo)}`
            // await axios.get(
            //   `/api/v2/users/oidc/callback?oidc_merge_state=${
            //     mergeState?.state_string
            //   }&redirect=${encodeURIComponent(redirectTo)}`,
            // )

            {
              /* <Link
          href={`/api/v2/users/oidc/callback?redirect=${encodeURIComponent(
            redirectTo,
          )}`}
        > */
            }
          }}
        ></SignInForm>
      </Maybe>
    </Section>
  )
}

export default AccountPage
