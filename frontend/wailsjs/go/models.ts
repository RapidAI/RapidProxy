export namespace main {
	
	export class AccessView {
	    baseUrl: string;
	    openaiUrl: string;
	    apiKeys: string[];
	    requireKey: boolean;
	    models: string[];
	    running: boolean;
	    sample: string;
	
	    static createFrom(source: any = {}) {
	        return new AccessView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.baseUrl = source["baseUrl"];
	        this.openaiUrl = source["openaiUrl"];
	        this.apiKeys = source["apiKeys"];
	        this.requireKey = source["requireKey"];
	        this.models = source["models"];
	        this.running = source["running"];
	        this.sample = source["sample"];
	    }
	}
	export class AccountView {
	    id: string;
	    provider: string;
	    providerName: string;
	    nickname: string;
	    uid: string;
	    enterpriseId: string;
	    expiresAt: string;
	    expiresIn: string;
	    expired: boolean;
	    createdAt: string;
	
	    static createFrom(source: any = {}) {
	        return new AccountView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.provider = source["provider"];
	        this.providerName = source["providerName"];
	        this.nickname = source["nickname"];
	        this.uid = source["uid"];
	        this.enterpriseId = source["enterpriseId"];
	        this.expiresAt = source["expiresAt"];
	        this.expiresIn = source["expiresIn"];
	        this.expired = source["expired"];
	        this.createdAt = source["createdAt"];
	    }
	}
	export class LoginView {
	    active: boolean;
	    provider: string;
	    url: string;
	    embedUrl: string;
	    state: string;
	    status: string;
	    message: string;
	    expiresAt: string;
	    accountId: string;
	
	    static createFrom(source: any = {}) {
	        return new LoginView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.active = source["active"];
	        this.provider = source["provider"];
	        this.url = source["url"];
	        this.embedUrl = source["embedUrl"];
	        this.state = source["state"];
	        this.status = source["status"];
	        this.message = source["message"];
	        this.expiresAt = source["expiresAt"];
	        this.accountId = source["accountId"];
	    }
	}
	export class ModelView {
	    id: string;
	    name: string;
	    provider: string;
	    context: number;
	    maxOut: number;
	    images: boolean;
	
	    static createFrom(source: any = {}) {
	        return new ModelView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.provider = source["provider"];
	        this.context = source["context"];
	        this.maxOut = source["maxOut"];
	        this.images = source["images"];
	    }
	}
	export class ProfileInput {
	    id: string;
	    enabled: boolean;
	    proxy: string;
	
	    static createFrom(source: any = {}) {
	        return new ProfileInput(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.enabled = source["enabled"];
	        this.proxy = source["proxy"];
	    }
	}
	export class ProviderView {
	    id: string;
	    name: string;
	    enabled: boolean;
	    baseUrl: string;
	    origin: string;
	    proxy: string;
	    accountCount: number;
	    modelCount: number;
	    needsProxy: boolean;
	
	    static createFrom(source: any = {}) {
	        return new ProviderView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.enabled = source["enabled"];
	        this.baseUrl = source["baseUrl"];
	        this.origin = source["origin"];
	        this.proxy = source["proxy"];
	        this.accountCount = source["accountCount"];
	        this.modelCount = source["modelCount"];
	        this.needsProxy = source["needsProxy"];
	    }
	}
	export class SettingsInput {
	    listen: string;
	    modelSyncHours: number;
	    cors: boolean;
	    sanitize: boolean;
	    maxThinking: boolean;
	    autoStart: boolean;
	    profiles: ProfileInput[];
	
	    static createFrom(source: any = {}) {
	        return new SettingsInput(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.listen = source["listen"];
	        this.modelSyncHours = source["modelSyncHours"];
	        this.cors = source["cors"];
	        this.sanitize = source["sanitize"];
	        this.maxThinking = source["maxThinking"];
	        this.autoStart = source["autoStart"];
	        this.profiles = this.convertValues(source["profiles"], ProfileInput);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class SettingsView {
	    listen: string;
	    modelSyncHours: number;
	    cors: boolean;
	    sanitize: boolean;
	    maxThinking: boolean;
	    autoStart: boolean;
	    configPath: string;
	    dataDir: string;
	    logPath: string;
	    accountDir: string;
	    version: string;
	    platform: string;
	
	    static createFrom(source: any = {}) {
	        return new SettingsView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.listen = source["listen"];
	        this.modelSyncHours = source["modelSyncHours"];
	        this.cors = source["cors"];
	        this.sanitize = source["sanitize"];
	        this.maxThinking = source["maxThinking"];
	        this.autoStart = source["autoStart"];
	        this.configPath = source["configPath"];
	        this.dataDir = source["dataDir"];
	        this.logPath = source["logPath"];
	        this.accountDir = source["accountDir"];
	        this.version = source["version"];
	        this.platform = source["platform"];
	    }
	}
	export class StateView {
	    running: boolean;
	    listen: string;
	    baseUrl: string;
	    openaiUrl: string;
	    requireKey: boolean;
	    apiKeys: string[];
	    providers: ProviderView[];
	    accounts: AccountView[];
	    models: ModelView[];
	    settings: SettingsView;
	    logs: string[];
	    login: LoginView;
	
	    static createFrom(source: any = {}) {
	        return new StateView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.running = source["running"];
	        this.listen = source["listen"];
	        this.baseUrl = source["baseUrl"];
	        this.openaiUrl = source["openaiUrl"];
	        this.requireKey = source["requireKey"];
	        this.apiKeys = source["apiKeys"];
	        this.providers = this.convertValues(source["providers"], ProviderView);
	        this.accounts = this.convertValues(source["accounts"], AccountView);
	        this.models = this.convertValues(source["models"], ModelView);
	        this.settings = this.convertValues(source["settings"], SettingsView);
	        this.logs = source["logs"];
	        this.login = this.convertValues(source["login"], LoginView);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

