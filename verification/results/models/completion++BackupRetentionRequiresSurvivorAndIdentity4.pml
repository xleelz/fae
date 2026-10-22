// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/backup_retention.go#L4
typedef Chandef {
	chan sync = [0] of {bool};
	chan enq = [0] of {bool};
	chan deq = [0] of {bool,bool};
	chan sending = [0] of {bool};
	chan rcving = [0] of {bool};
	chan closing = [0] of {bool};
	int size = 0;
	int num_msgs = 0;
	bool closed = false;
}



init { 
	chan child_BackupRetentionRequiresSurvivorAndIdentity40 = [1] of {int}->
	run BackupRetentionRequiresSurvivorAndIdentity4(child_BackupRetentionRequiresSurvivorAndIdentity40)->
	child_BackupRetentionRequiresSurvivorAndIdentity40?0->
stop_process:skip
}

proctype BackupRetentionRequiresSurvivorAndIdentity4(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_AnonymousBackupRetentionRequiresSurvivorAndIdentity1052 = [1] of {int}->
	chan child_AnonymousBackupRetentionRequiresSurvivorAndIdentity971 = [1] of {int}->
	chan child_AnonymousBackupRetentionRequiresSurvivorAndIdentity850 = [1] of {int}->
	Chandef removed_ch->
	Chandef unchanged_ch->
	Chandef survivor_ch->
	run sync_monitor(survivor_ch)->
	run sync_monitor(unchanged_ch)->
	run sync_monitor(removed_ch)->
	run AnonymousBackupRetentionRequiresSurvivorAndIdentity85(survivor_ch,unchanged_ch,removed_ch,child_AnonymousBackupRetentionRequiresSurvivorAndIdentity850)->
	run receiver(child_AnonymousBackupRetentionRequiresSurvivorAndIdentity850)->
	run AnonymousBackupRetentionRequiresSurvivorAndIdentity97(removed_ch,survivor_ch,unchanged_ch,child_AnonymousBackupRetentionRequiresSurvivorAndIdentity971)->
	run receiver(child_AnonymousBackupRetentionRequiresSurvivorAndIdentity971)->
	run AnonymousBackupRetentionRequiresSurvivorAndIdentity105(survivor_ch,unchanged_ch,removed_ch,child_AnonymousBackupRetentionRequiresSurvivorAndIdentity1052)->
	run receiver(child_AnonymousBackupRetentionRequiresSurvivorAndIdentity1052)->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/backup_retention.go:11:2 */
	:: removed_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/backup_retention.go:11:2 */->
	:: removed_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/backup_retention.go:11:2 */ -> 
		removed_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousBackupRetentionRequiresSurvivorAndIdentity85(Chandef survivor_ch;Chandef unchanged_ch;Chandef removed_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/backup_retention.go:8:14 */
	:: survivor_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/backup_retention.go:8:14 */
	:: survivor_ch.sync!false -> 
		survivor_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/backup_retention.go:8:14 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousBackupRetentionRequiresSurvivorAndIdentity97(Chandef removed_ch;Chandef survivor_ch;Chandef unchanged_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/backup_retention.go:9:14 */
	:: unchanged_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/backup_retention.go:9:14 */
	:: unchanged_ch.sync!false -> 
		unchanged_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/backup_retention.go:9:14 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousBackupRetentionRequiresSurvivorAndIdentity105(Chandef survivor_ch;Chandef unchanged_ch;Chandef removed_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/backup_retention.go:10:14 */
	:: survivor_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/backup_retention.go:10:14 */->
	:: survivor_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/backup_retention.go:10:14 */ -> 
		survivor_ch.rcving!false
	fi->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/backup_retention.go:10:26 */
	:: unchanged_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/backup_retention.go:10:26 */->
	:: unchanged_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/backup_retention.go:10:26 */ -> 
		unchanged_ch.rcving!false
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/backup_retention.go:10:39 */
	:: removed_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/backup_retention.go:10:39 */
	:: removed_ch.sync!false -> 
		removed_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/backup_retention.go:10:39 */
	fi->
	stop_process: skip->
	child!0
}

 /* ================================================================================== */
 /* ================================================================================== */
 /* ================================================================================== */ 
proctype async_monitor(Chandef ch) {
do
:: true ->
if
:: ch.closed -> 
end: if
  :: ch.num_msgs > 0 -> // cannot send on closed channel
    end4: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.deq!true,ch.num_msgs -> 
  		 ch.num_msgs = ch.num_msgs - 1
    fi->
  :: else ->    end5: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.sync!true-> 
    fi->
  fi->
:: else ->
	if
	:: ch.num_msgs == ch.size ->
		end1: if
		  :: ch.deq!false,ch.num_msgs ->
		    ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		    ch.closed = true
		   :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	:: ch.num_msgs == 0 -> 
end2:		if
		:: ch.enq!true -> // a message has been received
			ch.num_msgs = ch.num_msgs + 1
		:: ch.closing!false -> // closing the channel
			ch.closed = true
		:: ch.rcving?false ->
 		    ch.sending!true->
		fi->
		:: else -> 
		end3: if
		  :: ch.enq!true->
		     ch.num_msgs = ch.num_msgs + 1
		  :: ch.deq!false,ch.num_msgs->
		     ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		     ch.closed = true
		  :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	fi->
fi->
od->
}

proctype sync_monitor(Chandef ch) {
do
:: true->
if
:: ch.closed ->
end: if
  :: ch.enq!false-> // cannot send on closed channel
  :: ch.closing!true -> // cannot close twice a channel
  :: ch.sending!false -> // sending state of channel (closed)
  :: ch.rcving?false->
  :: ch.sync!true-> // can always receive on a closed chan
  fi->
:: else -> 
end1: if
    :: ch.rcving?false ->
       ch.sending!true->
    :: ch.closing!false ->
      ch.closed = true
    fi->
fi->
od->
stop_process: skip
}

proctype receiver(chan c) {
c?0
}

