export namespace backend {
	
	export class FileDTO {
	    path: string;
	    name: string;
	    extension: string;
	    sizeBytes: number;
	    modifiedAt: number;
	    snippet: string;
	    score?: number;
	
	    static createFrom(source: any = {}) {
	        return new FileDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.name = source["name"];
	        this.extension = source["extension"];
	        this.sizeBytes = source["sizeBytes"];
	        this.modifiedAt = source["modifiedAt"];
	        this.snippet = source["snippet"];
	        this.score = source["score"];
	    }
	}
	export class SuggestionDTO {
	    currentPath: string;
	    suggestedPath: string;
	    kind: string;
	    reason: string;
	    score: number;
	
	    static createFrom(source: any = {}) {
	        return new SuggestionDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.currentPath = source["currentPath"];
	        this.suggestedPath = source["suggestedPath"];
	        this.kind = source["kind"];
	        this.reason = source["reason"];
	        this.score = source["score"];
	    }
	}
	export class GroupDTO {
	    destination: string;
	    suggestions: SuggestionDTO[];
	
	    static createFrom(source: any = {}) {
	        return new GroupDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.destination = source["destination"];
	        this.suggestions = this.convertValues(source["suggestions"], SuggestionDTO);
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
	export class OrganizeReportDTO {
	    groups: GroupDTO[];
	    unmoved: SuggestionDTO[];
	    total: number;
	    folderCount: number;
	
	    static createFrom(source: any = {}) {
	        return new OrganizeReportDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.groups = this.convertValues(source["groups"], GroupDTO);
	        this.unmoved = this.convertValues(source["unmoved"], SuggestionDTO);
	        this.total = source["total"];
	        this.folderCount = source["folderCount"];
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
	export class ScanResult {
	    indexed: number;
	    database: string;
	
	    static createFrom(source: any = {}) {
	        return new ScanResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.indexed = source["indexed"];
	        this.database = source["database"];
	    }
	}

}

