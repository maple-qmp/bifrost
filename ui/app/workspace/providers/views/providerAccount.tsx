import { Button } from "@/components/ui/button";
import { ModelSelector } from "@/components/ui/modelSelector";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import {
	useGetProviderAccountQuery,
	useProviderAccountActionMutation,
	useUpdateProviderMutation,
	useRefreshProviderModelsMutation,
	providersApi,
} from "@/lib/store/apis/providersApi";
import { getErrorMessage, useAppDispatch } from "@/lib/store";
import { ModelProvider } from "@/lib/types/config";
import { localAccountSettingsSchema } from "@/lib/types/schemas";
import { zodResolver } from "@hookform/resolvers/zod";
import { useEffect, useState } from "react";
import { useForm } from "react-hook-form";
import { z } from "zod";
import { buildProviderUpdatePayload } from "./utils";

type Settings = z.infer<typeof localAccountSettingsSchema>;

export default function ProviderAccount({ provider, canUpdate }: { provider: ModelProvider; canUpdate: boolean }) {
	const {
		data,
		error: statusError,
		refetch,
	} = useGetProviderAccountQuery(provider.name, { pollingInterval: 2000, skipPollingIfUnfocused: true });
	const dispatch = useAppDispatch();
	const [refreshModels, { isLoading: isRefreshing }] = useRefreshProviderModelsMutation();
	useEffect(() => {
		if (data?.connected !== undefined) dispatch(providersApi.util.invalidateTags(["Models"]));
	}, [data?.connected, data?.identity, data?.login_state, dispatch]);
	const [action, { isLoading }] = useProviderAccountActionMutation();
	const [update, { isLoading: isSaving }] = useUpdateProviderMutation();
	const [login, setLogin] = useState<{ session_id: string; url: string }>();
	const [callback, setCallback] = useState("");
	const [model, setModel] = useState("");
	const [error, setError] = useState("");
	const form = useForm<Settings>({
		resolver: zodResolver(localAccountSettingsSchema),
		defaultValues: {
			credential_file: provider.local_account_config?.credential_file ?? "",
			prompt_compat: provider.local_account_config?.prompt_compat ?? true,
		},
	});
	const isChatGPT = provider.name === "chatgpt";
	const pending = data?.login_state === "pending" || data?.login_state === "exchanging";
	const run = async (operation: "login" | "detect" | "callback" | "disconnect") => {
		setError("");
		try {
			const result = await action({
				provider: provider.name,
				action: operation,
				...(operation === "callback" ? { session_id: login?.session_id, url: callback } : {}),
			}).unwrap();
			if (result.session_id && result.url) setLogin({ session_id: result.session_id, url: result.url });
			if (operation === "disconnect") {
				setLogin(undefined);
				setCallback("");
			}
			await refetch();
		} catch (err) {
			setError(getErrorMessage(err));
		}
	};
	const save = form.handleSubmit(async (values) => {
		setError("");
		try {
			await update(buildProviderUpdatePayload(provider, { local_account_config: values })).unwrap();
			await refetch();
		} catch (err) {
			setError(getErrorMessage(err));
		}
	});
	return (
		<section className="space-y-4 rounded-lg border p-4" data-testid="provider-account-panel">
			<div>
				<h2 className="text-lg font-medium">{isChatGPT ? "ChatGPT plan account" : "CodeBuddy desktop account"}</h2>
				<p className="text-muted-foreground text-sm">
					{isChatGPT
						? "Connect your ChatGPT account and approve plan usage. Available models and limits depend on your account."
						: "Sign in to CodeBuddy on this computer, then detect its desktop credentials."}
				</p>
			</div>
			<p role="status" data-testid="provider-account-status">
				{data?.connected ? `Connected${data.identity ? ` · ${data.identity}` : ""}` : "Disconnected"}
				{data?.expires_at ? ` · Token expires ${new Date(data.expires_at).toLocaleString()}` : ""}
				{pending ? " · Waiting for sign-in" : ""}
			</p>
			<div className="space-y-2">
				<div className="flex items-center justify-between">
					<Label htmlFor="account-model">Available models</Label>
					<Button
						variant="outline"
						size="sm"
						disabled={!canUpdate || !data?.connected || isRefreshing}
						data-testid="provider-account-refresh-models"
						onClick={async () => {
							setError("");
							try {
								await refreshModels(provider.name).unwrap();
							} catch (err) {
								setError(getErrorMessage(err));
							}
						}}
					>
						Refresh models
					</Button>
				</div>
				<ModelSelector
					inputId="account-model"
					provider={provider.name}
					value={model}
					onChange={setModel}
					data-testid="provider-account-model"
				/>
			</div>
			<div className="flex flex-wrap gap-2">
				<Button
					disabled={!canUpdate || isLoading || pending}
					onClick={() => run(isChatGPT ? "login" : "detect")}
					data-testid="provider-account-connect"
				>
					{isChatGPT ? (data?.connected ? "Reauthorize" : "Continue with ChatGPT") : "Detect account"}
				</Button>
				<Button
					variant="outline"
					disabled={!canUpdate || isLoading}
					onClick={() => run("disconnect")}
					data-testid="provider-account-disconnect"
				>
					Disconnect
				</Button>
			</div>
			{login && pending && (
				<div className="space-y-2">
					<a href={login.url} target="_blank" rel="noopener noreferrer" className="underline" data-testid="provider-account-login-link">
						Open ChatGPT sign-in
					</a>
					<Label htmlFor="account-callback">If the browser callback fails, paste the complete callback URL</Label>
					<Input
						id="account-callback"
						value={callback}
						onChange={(e) => setCallback(e.target.value)}
						data-testid="provider-account-callback"
						autoComplete="off"
					/>
					<Button
						variant="outline"
						disabled={!canUpdate || isLoading || !callback.trim()}
						onClick={() => run("callback")}
						data-testid="provider-account-complete"
					>
						Complete sign-in
					</Button>
				</div>
			)}
			{!isChatGPT && (
				<form onSubmit={save} className="space-y-2">
					<Label htmlFor="account-credential-file">Credential file (leave empty for desktop discovery)</Label>
					<Input
						id="account-credential-file"
						{...form.register("credential_file")}
						disabled={!canUpdate}
						data-testid="provider-account-credential-file"
					/>
					{form.formState.errors.credential_file && (
						<p className="text-destructive text-sm">{form.formState.errors.credential_file.message}</p>
					)}
					<div className="flex items-center gap-2">
						<Switch
							id="account-prompt-compat"
							checked={form.watch("prompt_compat")}
							onCheckedChange={(checked) => form.setValue("prompt_compat", checked)}
							disabled={!canUpdate}
							data-testid="provider-account-prompt-compat"
						/>
						<Label htmlFor="account-prompt-compat">System prompt compatibility</Label>
					</div>
					<Button type="submit" variant="outline" disabled={!canUpdate || isSaving} data-testid="provider-account-save">
						Save account settings
					</Button>
				</form>
			)}
			{Boolean(error || data?.error || statusError) && (
				<p role="alert" className="text-destructive text-sm" data-testid="provider-account-error">
					{error || data?.error || getErrorMessage(statusError)}
				</p>
			)}
		</section>
	);
}